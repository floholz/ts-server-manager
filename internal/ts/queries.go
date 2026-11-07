package ts

import (
	"fmt"
	"strconv"
)

// execFunc runs one command and returns its parsed rows.
type execFunc func(*Command) ([]Row, error)

// firstRow runs cmd and returns its first row, failing on an empty reply.
func firstRow(exec execFunc, cmd *Command) (Row, error) {
	rows, err := exec(cmd)
	if err != nil {
		return nil, err
	}
	if len(rows) == 0 {
		return nil, fmt.Errorf("%s: empty response", cmd.Name())
	}
	return rows[0], nil
}

func queryVersion(exec execFunc) (VersionInfo, error) {
	r, err := firstRow(exec, NewCommand("version"))
	if err != nil {
		return VersionInfo{}, fmt.Errorf("version: %w", err)
	}
	return VersionInfo{
		Version:  r.String("version"),
		Platform: r.String("platform"),
		Build:    r.IntOr("build", 0),
	}, nil
}

// queryServerInfo aggregates `version`, `whoami`, and `serverinfo`.
func queryServerInfo(exec execFunc) (ServerInfo, error) {
	v, err := queryVersion(exec)
	if err != nil {
		return ServerInfo{}, err
	}
	w, err := firstRow(exec, NewCommand("whoami"))
	if err != nil {
		return ServerInfo{}, fmt.Errorf("whoami: %w", err)
	}
	si, err := firstRow(exec, NewCommand("serverinfo"))
	if err != nil {
		return ServerInfo{}, fmt.Errorf("serverinfo: %w", err)
	}
	uptime, _ := strconv.ParseInt(si.String("virtualserver_uptime"), 10, 64)
	return ServerInfo{
		Version:       v.Version,
		Platform:      v.Platform,
		Build:         v.Build,
		SID:           w.IntOr("virtualserver_id", 0),
		ServerStatus:  w.String("virtualserver_status"),
		ServerName:    si.String("virtualserver_name"),
		UptimeSeconds: uptime,
		ClientsOnline: si.IntOr("virtualserver_clientsonline", 0),
		ClientsMax:    si.IntOr("virtualserver_maxclients", 0),
		LoginName:     w.String("client_login_name"),
	}, nil
}
