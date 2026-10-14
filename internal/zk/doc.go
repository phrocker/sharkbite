// Package zk wraps github.com/go-zookeeper/zk and resolves the root-tablet
// tserver location from the bootstrap chain
// /accumulo/instances/<name> → instance UUID → /accumulo/<uuid>/root_tablet
// (JSON RootTabletMetadata). Reference:
// core/.../client/clientImpl/RootClientTabletCache.java.
package zk
