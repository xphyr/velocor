package spdk

import (
	"context"
	"errors"
)

// IscsiPortal represents an IP:Port endpoint inside an SPDK iSCSI Portal Group.
type IscsiPortal struct {
	Host string `json:"host"`
	Port string `json:"port"`
}

// IscsiPortalGroup describes an SPDK iSCSI portal group.
type IscsiPortalGroup struct {
	Tag     int           `json:"tag"`
	Portals []IscsiPortal `json:"portals"`
}

// IscsiInitiatorGroup describes an SPDK iSCSI initiator ACL group.
type IscsiInitiatorGroup struct {
	Tag        int      `json:"tag"`
	Initiators []string `json:"initiators"`
	Netmasks   []string `json:"netmasks"`
}

// IscsiPgIgMap pairs a portal group tag with an initiator group tag on an iSCSI target node.
type IscsiPgIgMap struct {
	PgTag int `json:"pg_tag"`
	IgTag int `json:"ig_tag"`
}

// IscsiLun describes a LUN attached to an SPDK iSCSI target node.
type IscsiLun struct {
	BdevName string `json:"bdev_name"`
	LunID    int    `json:"lun_id"`
}

// IscsiTargetNode describes an SPDK iSCSI target node returned by iscsi_get_target_nodes.
type IscsiTargetNode struct {
	Name        string         `json:"name"`
	AliasName   string         `json:"alias_name"`
	PgIgMaps    []IscsiPgIgMap `json:"pg_ig_maps"`
	Luns        []IscsiLun     `json:"luns"`
	QueueDepth  int            `json:"queue_depth"`
	DisableChap bool           `json:"disable_chap"`
	RequireChap bool           `json:"require_chap"`
	MutualChap  bool           `json:"mutual_chap"`
	ChapGroup   int            `json:"chap_group"`
}

// IscsiGetPortalGroups queries SPDK for all iSCSI portal groups.
func (c *Client) IscsiGetPortalGroups(ctx context.Context) ([]IscsiPortalGroup, error) {
	var res []IscsiPortalGroup
	if err := c.Call(ctx, "iscsi_get_portal_groups", nil, &res); err != nil {
		return nil, err
	}
	return res, nil
}

// IscsiCreatePortalGroup creates an iSCSI portal group with the given numeric tag and portals.
func (c *Client) IscsiCreatePortalGroup(ctx context.Context, tag int, portals []IscsiPortal) error {
	params := map[string]any{
		"tag":     tag,
		"portals": portals,
	}
	return c.Call(ctx, "iscsi_create_portal_group", params, nil)
}

// IscsiDeletePortalGroup removes an iSCSI portal group by tag.
func (c *Client) IscsiDeletePortalGroup(ctx context.Context, tag int) error {
	params := map[string]any{"tag": tag}
	return c.Call(ctx, "iscsi_delete_portal_group", params, nil)
}

// IscsiGetInitiatorGroups queries SPDK for all iSCSI initiator groups.
func (c *Client) IscsiGetInitiatorGroups(ctx context.Context) ([]IscsiInitiatorGroup, error) {
	var res []IscsiInitiatorGroup
	if err := c.Call(ctx, "iscsi_get_initiator_groups", nil, &res); err != nil {
		return nil, err
	}
	return res, nil
}

// IscsiCreateInitiatorGroup creates an iSCSI initiator group with allowed IQNs ("ANY" or list) and CIDR netmasks.
func (c *Client) IscsiCreateInitiatorGroup(ctx context.Context, tag int, initiators, netmasks []string) error {
	// Never silently widen an ACL: callers must state "ANY" explicitly.
	if len(initiators) == 0 || len(netmasks) == 0 {
		return errors.New("iscsi initiator group requires at least one initiator and one netmask")
	}
	params := map[string]any{
		"tag":        tag,
		"initiators": initiators,
		"netmasks":   netmasks,
	}
	return c.Call(ctx, "iscsi_create_initiator_group", params, nil)
}

// IscsiDeleteInitiatorGroup removes an iSCSI initiator group by tag.
func (c *Client) IscsiDeleteInitiatorGroup(ctx context.Context, tag int) error {
	params := map[string]any{"tag": tag}
	return c.Call(ctx, "iscsi_delete_initiator_group", params, nil)
}

// IscsiGetTargetNodes queries SPDK for all configured iSCSI target nodes.
func (c *Client) IscsiGetTargetNodes(ctx context.Context) ([]IscsiTargetNode, error) {
	var res []IscsiTargetNode
	if err := c.Call(ctx, "iscsi_get_target_nodes", nil, &res); err != nil {
		return nil, err
	}
	return res, nil
}

// IscsiCreateTargetNode creates an iSCSI target node exposing the specified LUNs and Portal/Initiator group bindings.
func (c *Client) IscsiCreateTargetNode(
	ctx context.Context,
	name, aliasName string,
	luns []IscsiLun,
	pgIgMaps []IscsiPgIgMap,
	queueDepth int,
	disableChap, requireChap, mutualChap bool,
	chapGroup int,
) error {
	if queueDepth <= 0 {
		queueDepth = 64
	}
	params := map[string]any{
		"name":         name,
		"alias_name":   aliasName,
		"luns":         luns,
		"pg_ig_maps":   pgIgMaps,
		"queue_depth":  queueDepth,
		"disable_chap": disableChap,
		"require_chap": requireChap,
		"mutual_chap":  mutualChap,
		"chap_group":   chapGroup,
	}
	return c.Call(ctx, "iscsi_create_target_node", params, nil)
}

// IscsiDeleteTargetNode removes an iSCSI target node by IQN name.
func (c *Client) IscsiDeleteTargetNode(ctx context.Context, name string) error {
	params := map[string]any{"name": name}
	return c.Call(ctx, "iscsi_delete_target_node", params, nil)
}

// IscsiTargetNodeAddPgIgMaps adds portal-group/initiator-group mappings to an existing target node
// without disrupting active sessions.
func (c *Client) IscsiTargetNodeAddPgIgMaps(ctx context.Context, name string, maps []IscsiPgIgMap) error {
	params := map[string]any{"name": name, "pg_ig_maps": maps}
	return c.Call(ctx, "iscsi_target_node_add_pg_ig_maps", params, nil)
}

// IscsiTargetNodeRemovePgIgMaps removes portal-group/initiator-group mappings from a target node.
func (c *Client) IscsiTargetNodeRemovePgIgMaps(ctx context.Context, name string, maps []IscsiPgIgMap) error {
	params := map[string]any{"name": name, "pg_ig_maps": maps}
	return c.Call(ctx, "iscsi_target_node_remove_pg_ig_maps", params, nil)
}

// IscsiInitiatorGroupAddInitiators adds initiators/netmasks to an existing initiator group.
func (c *Client) IscsiInitiatorGroupAddInitiators(ctx context.Context, tag int, initiators, netmasks []string) error {
	params := map[string]any{"tag": tag}
	if len(initiators) > 0 {
		params["initiators"] = initiators
	}
	if len(netmasks) > 0 {
		params["netmasks"] = netmasks
	}
	return c.Call(ctx, "iscsi_initiator_group_add_initiators", params, nil)
}

// IscsiInitiatorGroupRemoveInitiators removes initiators/netmasks from an existing initiator group.
func (c *Client) IscsiInitiatorGroupRemoveInitiators(ctx context.Context, tag int, initiators, netmasks []string) error {
	params := map[string]any{"tag": tag}
	if len(initiators) > 0 {
		params["initiators"] = initiators
	}
	if len(netmasks) > 0 {
		params["netmasks"] = netmasks
	}
	return c.Call(ctx, "iscsi_initiator_group_remove_initiators", params, nil)
}

// IscsiConnection is an active iSCSI session connection.
type IscsiConnection struct {
	ID             int    `json:"id"`
	InitiatorAddr  string `json:"initiator_addr"`
	TargetAddr     string `json:"target_addr"`
	TargetNodeName string `json:"target_node_name"`
}

// IscsiGetConnections lists active iSCSI connections.
func (c *Client) IscsiGetConnections(ctx context.Context) ([]IscsiConnection, error) {
	var res []IscsiConnection
	if err := c.Call(ctx, "iscsi_get_connections", nil, &res); err != nil {
		return nil, err
	}
	return res, nil
}
