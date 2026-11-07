package backup

// Restore allowlists, derived from the TS6 6.0.0-beta13.1 verification in
// the spec's Appendix A. Only these properties are ever written to a
// target; everything else in a backup is informational.

// restorableServerProps are written with serveredit. Deliberately absent:
// port, address, machine ID, status, autostart, password, SFU endpoint,
// maxclients (license dependent), bandwidth/quotas (instance specific),
// default group pointers (written separately with mapped IDs).
var restorableServerProps = []string{
	// identity & messages
	"virtualserver_name",
	"virtualserver_name_phonetic",
	"virtualserver_nickname",
	"virtualserver_welcomemessage",
	"virtualserver_hostmessage",
	"virtualserver_hostmessage_mode",
	// branding
	"virtualserver_hostbanner_url",
	"virtualserver_hostbanner_gfx_url",
	"virtualserver_hostbanner_gfx_interval",
	"virtualserver_hostbanner_mode",
	"virtualserver_hostbutton_tooltip",
	"virtualserver_hostbutton_url",
	"virtualserver_hostbutton_gfx_url",
	"virtualserver_icon_id",
	// behaviour
	"virtualserver_codec_encryption_mode",
	"virtualserver_channel_temp_delete_delay_default",
	"virtualserver_priority_speaker_dimm_modificator",
	"virtualserver_min_clients_in_channel_before_forced_silence",
	"virtualserver_needed_identity_security_level",
	"virtualserver_min_client_version",
	"virtualserver_min_android_version",
	"virtualserver_min_ios_version",
	"virtualserver_mytsid_connect_only",
	"virtualserver_weblist_enabled",
	"virtualserver_reserved_slots",
	// anti-flood & complaints
	"virtualserver_antiflood_points_tick_reduce",
	"virtualserver_antiflood_points_needed_command_block",
	"virtualserver_antiflood_points_needed_ip_block",
	"virtualserver_antiflood_points_needed_plugin_block",
	"virtualserver_complain_autoban_count",
	"virtualserver_complain_autoban_time",
	"virtualserver_complain_remove_time",
	// logging
	"virtualserver_log_client",
	"virtualserver_log_query",
	"virtualserver_log_channel",
	"virtualserver_log_permissions",
	"virtualserver_log_server",
	"virtualserver_log_filetransfer",
}

// restorableChannelProps are sent with channelcreate. Not included:
// channel_name / cpid / channel_order (sent explicitly), channel_password
// (handled separately), channel_flag_default (set after the tree exists;
// TS6 only accepts setting it to 1), and read-only properties.
var restorableChannelProps = []string{
	"channel_name_phonetic",
	"channel_topic",
	"channel_description",
	"channel_codec",
	"channel_codec_quality",
	"channel_codec_latency_factor",
	"channel_codec_is_unencrypted",
	"channel_maxclients",
	"channel_maxfamilyclients",
	"channel_flag_maxclients_unlimited",
	"channel_flag_maxfamilyclients_unlimited",
	"channel_flag_maxfamilyclients_inherited",
	"channel_flag_permanent",
	"channel_flag_semi_permanent",
	"channel_needed_talk_power",
	"channel_storage_quota",
	"channel_banner_gfx_url",
	"channel_banner_mode",
}

// Stock regular group names on a fresh TS6 virtual server.
var (
	stockServerGroups  = map[string]bool{"Server Admin": true, "Normal": true, "Guest": true}
	stockChannelGroups = map[string]bool{"Channel Admin": true, "Operator": true, "Voice": true, "Guest": true}
)
