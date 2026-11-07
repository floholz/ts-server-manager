package backup

import (
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/floholz/ts-server-manager/internal/ts"
)

// simServer is an in-memory TS6 virtual server for restore tests. It
// implements the subset of ServerQuery that export and restore use, with
// the behaviours observed on 6.0.0-beta13.1 (spec Appendix A).
type simServer struct {
	props    map[string]string
	sgs, cgs []*simGroup
	chans    []*simChannel // in no particular order; tree via pid + order
	nextSG   int
	nextCG   int
	nextCID  int
	known    map[string]bool
	writes   []string // modifying commands received
	failNext map[string]bool
}

type simGroup struct {
	id    int
	name  string
	typ   string
	perms map[string]Perm
}

type simChannel struct {
	cid, pid int
	order    int // cid of the sibling above, 0 = first
	name     string
	props    map[string]string
	perms    map[string]Perm
}

// simHash mimics TS6's channel password hash: deterministic per server,
// different across servers. On the fixture's server, "s3cret" hashes to
// the value captured from a real server.
func (s *simServer) simHash(pw string) string {
	uid := s.props["virtualserver_unique_identifier"]
	if uid == fixtureUID && pw == "s3cret" {
		return "enDElJk5HEDQo7PmB469eMoCfSs="
	}
	return "H:" + uid + ":" + pw
}

// permsNotValidOnChannels are rejected by channeladdperm with "invalid
// parameter" after the preceding batch items were applied.
var permsNotValidOnChannels = map[string]bool{"b_channel_join_ignore_password": true}

func newFreshSim() *simServer {
	s := &simServer{
		props: map[string]string{
			"virtualserver_name":                        "TeamSpeak 6 Server",
			"virtualserver_unique_identifier":           fixtureUID,
			"virtualserver_welcomemessage":              "Welcome to TeamSpeak",
			"virtualserver_port":                        "9987",
			"virtualserver_clientsonline":               "1",
			"virtualserver_queryclientsonline":          "1",
			"virtualserver_default_server_group":        "8",
			"virtualserver_default_channel_group":       "8",
			"virtualserver_default_channel_admin_group": "5",
		},
		nextSG: 9, nextCG: 9, nextCID: 2,
		known: map[string]bool{},
	}
	for _, p := range []string{
		"b_virtualserver_modify_name", "b_virtualserver_channel_list", "i_group_sort_id",
		"i_client_talk_power", "i_client_kick_from_channel_power", "b_channel_modify_name",
		"i_channel_needed_permission_modify_power", "i_channel_needed_join_power", "i_icon_id",
		"i_channel_needed_delete_power", "b_group_is_permanent", "i_group_needed_modify_power",
		"b_channel_join_ignore_password", "b_client_ignore_antiflood",
	} {
		s.known[p] = true
	}
	add := func(list *[]*simGroup, id int, name, typ string, perms ...Perm) {
		g := &simGroup{id: id, name: name, typ: typ, perms: map[string]Perm{}}
		for _, p := range perms {
			g.perms[p.PermSID] = p
		}
		*list = append(*list, g)
	}
	add(&s.sgs, 1, "Guest Server Query", "2")
	add(&s.sgs, 2, "Admin Server Query", "2")
	add(&s.sgs, 3, "Server Admin", "0")
	add(&s.sgs, 4, "Normal", "0")
	add(&s.sgs, 5, "Guest", "0")
	add(&s.sgs, 6, "Server Admin", "1", Perm{PermSID: "b_virtualserver_modify_name", Value: 1}, Perm{PermSID: "b_client_ignore_antiflood", Value: 1})
	add(&s.sgs, 7, "Normal", "1", Perm{PermSID: "b_virtualserver_channel_list", Value: 1})
	add(&s.sgs, 8, "Guest", "1", Perm{PermSID: "b_virtualserver_channel_list", Value: 1})
	for i, n := range []string{"Channel Admin", "Operator", "Voice", "Guest"} {
		add(&s.cgs, i+1, n, "0")
		add(&s.cgs, i+5, n, "1")
	}
	s.chans = []*simChannel{{
		cid: 1, name: "Default Channel",
		props: map[string]string{"channel_flag_default": "1", "channel_flag_permanent": "1", "channel_topic": "Default Channel has no topic"},
		perms: map[string]Perm{"i_channel_needed_permission_modify_power": {PermSID: "i_channel_needed_permission_modify_power", Value: 50}},
	}}
	return s
}

func qerr(id int, msg string) error { return &ts.QueryError{ID: id, Msg: msg} }

func (s *simServer) Exec(cmd *ts.Command) ([]ts.Row, error) {
	line := cmd.String()
	name, rest, _ := strings.Cut(line, " ")
	var items []ts.Row
	options := map[string]bool{}
	for _, row := range ts.ParseRows([]string{rest}) {
		item := ts.Row{}
		for k, v := range row {
			if strings.HasPrefix(k, "-") {
				options[k] = true
			} else {
				item[k] = v
			}
		}
		if len(item) > 0 {
			items = append(items, item)
		}
	}
	args := ts.Row{}
	if len(items) > 0 {
		args = items[0]
	}

	switch name {
	case "version", "serverinfo", "servergrouplist", "channelgrouplist", "channellist",
		"permissionlist", "channelinfo", "servergrouppermlist", "channelgrouppermlist", "channelpermlist":
		return s.read(name, args)
	}
	s.writes = append(s.writes, line)
	if s.failNext[name] {
		delete(s.failNext, name)
		return nil, qerr(9999, "injected failure")
	}
	return s.write(name, args, items)
}

func (s *simServer) read(name string, args ts.Row) ([]ts.Row, error) {
	switch name {
	case "version":
		return []ts.Row{{"version": "6.0.0-beta13.1", "platform": "Linux", "build": "1"}}, nil
	case "serverinfo":
		r := ts.Row{}
		for k, v := range s.props {
			r[k] = v
		}
		return []ts.Row{r}, nil
	case "servergrouplist", "channelgrouplist":
		list, key := s.sgs, "sgid"
		if name == "channelgrouplist" {
			list, key = s.cgs, "cgid"
		}
		var rows []ts.Row
		for _, g := range list {
			rows = append(rows, ts.Row{key: strconv.Itoa(g.id), "name": g.name, "type": g.typ})
		}
		return rows, nil
	case "permissionlist":
		var rows []ts.Row
		for p := range s.known {
			rows = append(rows, ts.Row{"permname": p})
		}
		return rows, nil
	case "channellist":
		var rows []ts.Row
		for _, c := range s.chans {
			rows = append(rows, ts.Row{
				"cid": strconv.Itoa(c.cid), "pid": strconv.Itoa(c.pid), "channel_order": strconv.Itoa(c.order),
				"channel_name": c.name, "channel_flag_default": orZero(c.props["channel_flag_default"]),
			})
		}
		return rows, nil
	case "channelinfo":
		c := s.channel(args["cid"])
		if c == nil {
			return nil, qerr(768, "invalid channelID")
		}
		r := ts.Row{"channel_name": c.name, "pid": strconv.Itoa(c.pid), "channel_order": strconv.Itoa(c.order),
			"channel_flag_password": "0", "channel_password": ""}
		for k, v := range c.props {
			r[k] = v
		}
		r["channel_flag_default"] = orZero(c.props["channel_flag_default"])
		return []ts.Row{r}, nil
	case "servergrouppermlist", "channelgrouppermlist", "channelpermlist":
		var perms map[string]Perm
		switch name {
		case "servergrouppermlist":
			g := findGroup(s.sgs, args["sgid"])
			if g == nil {
				return nil, qerr(2560, "invalid group ID")
			}
			perms = g.perms
		case "channelgrouppermlist":
			g := findGroup(s.cgs, args["cgid"])
			if g == nil {
				return nil, qerr(2560, "invalid group ID")
			}
			perms = g.perms
		default:
			c := s.channel(args["cid"])
			if c == nil {
				return nil, qerr(768, "invalid channelID")
			}
			perms = c.perms
		}
		var rows []ts.Row
		for _, p := range perms {
			rows = append(rows, ts.Row{"permsid": p.PermSID, "permvalue": strconv.Itoa(p.Value),
				"permnegated": boolStr(p.Negated), "permskip": boolStr(p.Skip)})
		}
		sort.Slice(rows, func(i, j int) bool { return rows[i]["permsid"] < rows[j]["permsid"] })
		return rows, nil
	}
	return nil, fmt.Errorf("sim: unhandled read %s", name)
}

func (s *simServer) write(name string, args ts.Row, items []ts.Row) ([]ts.Row, error) {
	switch name {
	case "serveredit":
		for k := range args {
			if k == "virtualserver_bogus" {
				return nil, qerr(1538, "invalid parameter")
			}
		}
		for k, v := range args {
			s.props[k] = v
		}
		return nil, nil

	case "servergroupadd", "channelgroupadd":
		list, next, key := &s.sgs, &s.nextSG, "sgid"
		if name == "channelgroupadd" {
			list, next, key = &s.cgs, &s.nextCG, "cgid"
		}
		for _, g := range *list {
			if g.typ == "1" && g.name == args["name"] {
				return nil, qerr(1282, "database duplicate entry")
			}
		}
		g := &simGroup{id: *next, name: args["name"], typ: "1", perms: map[string]Perm{
			"b_group_is_permanent":        {PermSID: "b_group_is_permanent", Value: 1},
			"i_group_needed_modify_power": {PermSID: "i_group_needed_modify_power", Value: 100},
		}}
		*next++
		*list = append(*list, g)
		return []ts.Row{{key: strconv.Itoa(g.id)}}, nil

	case "servergrouprename", "channelgrouprename":
		g := findGroup(s.sgs, args["sgid"])
		if name == "channelgrouprename" {
			g = findGroup(s.cgs, args["cgid"])
		}
		if g == nil {
			return nil, qerr(2560, "invalid group ID")
		}
		g.name = args["name"]
		return nil, nil

	case "servergroupdel", "channelgroupdel":
		list, key, defaults := &s.sgs, "sgid", []string{s.props["virtualserver_default_server_group"]}
		if name == "channelgroupdel" {
			list, key, defaults = &s.cgs, "cgid",
				[]string{s.props["virtualserver_default_channel_group"], s.props["virtualserver_default_channel_admin_group"]}
		}
		for _, d := range defaults {
			if d == args[key] {
				return nil, qerr(2564, "access to default group is forbidden")
			}
		}
		for i, g := range *list {
			if strconv.Itoa(g.id) == args[key] {
				*list = append((*list)[:i], (*list)[i+1:]...)
				return nil, nil
			}
		}
		return nil, qerr(2560, "invalid group ID")

	case "servergroupaddperm", "channelgroupaddperm", "channeladdperm",
		"servergroupdelperm", "channelgroupdelperm", "channeldelperm":
		return s.perms(name, args, items)

	case "channelcreate":
		pid := atoi(args["cpid"])
		if pid != 0 && s.channel(args["cpid"]) == nil {
			return nil, qerr(768, "invalid channelID")
		}
		for _, c := range s.chans {
			if c.pid == pid && c.name == args["channel_name"] {
				return nil, qerr(771, "channel name is already in use")
			}
		}
		c := &simChannel{cid: s.nextCID, pid: pid, name: args["channel_name"], props: map[string]string{},
			perms: map[string]Perm{"i_channel_needed_delete_power": {PermSID: "i_channel_needed_delete_power", Value: 100}}}
		s.nextCID++
		for k, v := range args {
			switch k {
			case "channel_name", "cpid", "channel_order":
			case "channel_password":
				c.props["channel_password"] = s.simHash(v)
				c.props["channel_flag_password"] = "1"
			default:
				c.props[k] = v
			}
		}
		s.insert(c, atoi(args["channel_order"]))
		return []ts.Row{{"cid": strconv.Itoa(c.cid)}}, nil

	case "channeledit":
		c := s.channel(args["cid"])
		if c == nil {
			return nil, qerr(768, "invalid channelID")
		}
		for k, v := range args {
			switch k {
			case "cid":
			case "channel_name":
				c.name = v
			case "channel_flag_default":
				if v != "1" {
					return nil, qerr(1538, "invalid parameter")
				}
				for _, o := range s.chans {
					delete(o.props, "channel_flag_default")
				}
				c.props["channel_flag_default"] = "1"
			default:
				c.props[k] = v
			}
		}
		return nil, nil

	case "channeldelete":
		c := s.channel(args["cid"])
		if c == nil {
			return nil, qerr(768, "invalid channelID")
		}
		if c.props["channel_flag_default"] == "1" {
			return nil, qerr(775, "invalid channel flags")
		}
		s.deleteTree(c.cid)
		return nil, nil
	}
	return nil, fmt.Errorf("sim: unhandled write %s", name)
}

func (s *simServer) perms(name string, args ts.Row, items []ts.Row) ([]ts.Row, error) {
	var target map[string]Perm
	isChannel := strings.HasPrefix(name, "channeladd") || strings.HasPrefix(name, "channeldel")
	switch {
	case strings.HasPrefix(name, "servergroup"):
		g := findGroup(s.sgs, args["sgid"])
		if g == nil {
			return nil, qerr(2560, "invalid group ID")
		}
		target = g.perms
	case strings.HasPrefix(name, "channelgroup"):
		g := findGroup(s.cgs, args["cgid"])
		if g == nil {
			return nil, qerr(2560, "invalid group ID")
		}
		target = g.perms
	default:
		c := s.channel(args["cid"])
		if c == nil {
			return nil, qerr(768, "invalid channelID")
		}
		target = c.perms
	}
	// An unknown permission rejects the whole batch.
	for _, it := range items {
		if !s.known[it["permsid"]] {
			return nil, qerr(2562, "invalid permission ID")
		}
	}
	adding := strings.Contains(name, "addperm")
	for _, it := range items {
		sid := it["permsid"]
		if !adding {
			delete(target, sid)
			continue
		}
		// Context-invalid permission: earlier items stay applied.
		if isChannel && permsNotValidOnChannels[sid] {
			return nil, qerr(1538, "invalid parameter")
		}
		if !isChannel && (it["permnegated"] == "" || it["permskip"] == "") {
			return nil, qerr(1539, "parameter not found")
		}
		target[sid] = Perm{PermSID: sid, Value: atoi(it["permvalue"]),
			Negated: it["permnegated"] == "1", Skip: it["permskip"] == "1"}
	}
	return nil, nil
}

func (s *simServer) channel(cid string) *simChannel {
	for _, c := range s.chans {
		if strconv.Itoa(c.cid) == cid {
			return c
		}
	}
	return nil
}

// insert places c after sibling `after` (0 = first) and re-links orders.
func (s *simServer) insert(c *simChannel, after int) {
	sibs := s.siblings(c.pid)
	pos := 0
	for i, sib := range sibs {
		if sib.cid == after {
			pos = i + 1
		}
	}
	sibs = append(sibs[:pos], append([]*simChannel{c}, sibs[pos:]...)...)
	s.chans = append(s.chans, c)
	s.relink(sibs)
}

func (s *simServer) siblings(pid int) []*simChannel {
	var sibs []*simChannel
	for _, c := range s.chans {
		if c.pid == pid {
			sibs = append(sibs, c)
		}
	}
	// order by the channel_order chain
	ordered := []*simChannel{}
	prev := 0
	for len(ordered) < len(sibs) {
		found := false
		for _, c := range sibs {
			if c.order == prev {
				ordered = append(ordered, c)
				prev = c.cid
				found = true
				break
			}
		}
		if !found {
			break
		}
	}
	return ordered
}

func (s *simServer) relink(sibs []*simChannel) {
	prev := 0
	for _, c := range sibs {
		c.order = prev
		prev = c.cid
	}
}

func (s *simServer) deleteTree(cid int) {
	var victim *simChannel
	for _, c := range s.chans {
		if c.cid == cid {
			victim = c
		}
	}
	for _, c := range append([]*simChannel(nil), s.chans...) {
		if c.pid == cid {
			s.deleteTree(c.cid)
		}
	}
	sibs := s.siblings(victim.pid)
	var keep []*simChannel
	for _, c := range s.chans {
		if c.cid != cid {
			keep = append(keep, c)
		}
	}
	s.chans = keep
	var rest []*simChannel
	for _, c := range sibs {
		if c.cid != cid {
			rest = append(rest, c)
		}
	}
	s.relink(rest)
}

func findGroup(list []*simGroup, id string) *simGroup {
	for _, g := range list {
		if strconv.Itoa(g.id) == id {
			return g
		}
	}
	return nil
}

func atoi(s string) int { n, _ := strconv.Atoi(s); return n }

func boolStr(b bool) string {
	if b {
		return "1"
	}
	return "0"
}

func orZero(s string) string {
	if s == "" {
		return "0"
	}
	return s
}

func toRows(in []map[string]string) []ts.Row {
	out := make([]ts.Row, len(in))
	for i, m := range in {
		out[i] = ts.Row(m)
	}
	return out
}
