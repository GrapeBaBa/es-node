package p2p

import (
	"context"
	"errors"
	"fmt"
	"strconv"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/ethdb"
	"github.com/ethereum/go-ethereum/event"
	"github.com/ethereum/go-ethereum/log"
	"github.com/ethereum/go-ethereum/p2p/discover"
	"github.com/ethereum/go-ethereum/p2p/enode"
	"github.com/ethstorage/go-ethstorage/ethstorage"
	"github.com/ethstorage/go-ethstorage/ethstorage/p2p/protocol"
	"github.com/ethstorage/go-ethstorage/ethstorage/rollup"
	"github.com/hashicorp/go-multierror"
	pubsub "github.com/libp2p/go-libp2p-pubsub"
	"github.com/libp2p/go-libp2p/core/connmgr"
	"github.com/libp2p/go-libp2p/core/host"
	p2pmetrics "github.com/libp2p/go-libp2p/core/metrics"
	"github.com/libp2p/go-libp2p/core/network"
	ma "github.com/multiformats/go-multiaddr"
)

// NodeP2P is a p2p node, which can be used to gossip messages.
type NodeP2P struct {
	host    host.Host           // p2p host (optional, may be nil)
	gater   ConnectionGater     // p2p gater, to ban/unban peers with, may be nil even with p2p enabled
	connMgr connmgr.ConnManager // p2p conn manager, to keep a reliable number of peers, may be nil even with p2p enabled
	// the below components are all optional, and may be nil. They require the host to not be nil.
	dv5Local       *enode.LocalNode // p2p discovery identity
	dv5Udp         *discover.UDPv5  // p2p discovery service
	gs             *pubsub.PubSub   // p2p gossip router
	syncCl         *protocol.SyncClient
	syncSrv        *protocol.SyncServer
	storageManager *ethstorage.StorageManager
}

type Metricer interface {
	RecordGossipEvent(evType int32)
	// Peer Scoring Metric Funcs
	SetPeerScores(map[string]float64)
}

// NewNodeP2P creates a new p2p node, and returns a reference to it. If the p2p is disabled, it returns nil.
// If metrics are configured, a bandwidth monitor will be spawned in a goroutine.
func NewNodeP2P(resourcesCtx context.Context, rollupCfg *rollup.EsConfig, l1ChainID uint64, log log.Logger, setup SetupP2P,
	storageManager *ethstorage.StorageManager, db ethdb.Database, metrics Metricer, feed *event.Feed) (*NodeP2P, error) {
	if setup == nil {
		return nil, errors.New("p2p node cannot be created without setup")
	}
	var n NodeP2P
	if err := n.init(resourcesCtx, rollupCfg, l1ChainID, log, setup, storageManager, db, metrics, feed); err != nil {
		closeErr := n.Close()
		if closeErr != nil {
			log.Error("failed to close p2p after starting with err", "closeErr", closeErr, "err", err)
		}
		return nil, err
	}
	if n.host == nil {
		return nil, nil
	}
	return &n, nil
}

func (n *NodeP2P) init(resourcesCtx context.Context, rollupCfg *rollup.EsConfig, l1ChainID uint64, log log.Logger, setup SetupP2P,
	storageManager *ethstorage.StorageManager, db ethdb.Database, metrics Metricer, feed *event.Feed) error {
	bwc := p2pmetrics.NewBandwidthCounter()
	n.storageManager = storageManager

	var err error
	// nil if disabled.
	n.host, err = setup.Host(log, bwc)
	if err != nil {
		if n.dv5Udp != nil {
			n.dv5Udp.Close()
		}
		return fmt.Errorf("failed to start p2p host: %w", err)
	}

	if n.host != nil {
		// Enable extra features, if any. During testing we don't setup the most advanced host all the time.
		if extra, ok := n.host.(ExtraHostFeatures); ok {
			n.gater = extra.ConnectionGater()
			n.connMgr = extra.ConnectionManager()
		}
		m := (protocol.Metricer)(nil)
		if rollupCfg.MetricsEnable {
			m = protocol.NewMetrics("sync")
		}
		// Activate the P2P req-resp sync
		// TODO: add mux to through out a sync done event for mining later
		n.syncCl = protocol.NewSyncClient(log, rollupCfg, n.host.NewStream, storageManager, db, m, feed)
		n.host.Network().Notify(&network.NotifyBundle{
			ConnectedF: func(nw network.Network, conn network.Conn) {
				shards := make(map[common.Address][]uint64)
				css, err := n.Host().Peerstore().Get(conn.RemotePeer(), protocol.EthStorageENRKey)
				if err != nil {
					log.Warn("get shards from peer failed", "error", err.Error(), "peer", conn.RemotePeer())
					conn.Close()
					return
				} else {
					shards = protocol.ConvertToShardList(css.([]*protocol.ContractShards))
				}
				added := n.syncCl.AddPeer(conn.RemotePeer(), shards)
				if !added {
					conn.Close()
				}
			},
			DisconnectedF: func(nw network.Network, conn network.Conn) {
				n.syncCl.RemovePeer(conn.RemotePeer())
			},
		})
		n.syncCl.UpdateMaxPeers(int(setup.(*Config).PeersHi))
		// the host may already be connected to peers, add them all to the sync client
		for _, conn := range n.host.Network().Conns() {
			shards := make(map[common.Address][]uint64)
			css, err := n.host.Peerstore().Get(conn.RemotePeer(), protocol.EthStorageENRKey)
			if err != nil {
				log.Warn("get shards from peer failed", "error", err.Error(), "peer", conn.RemotePeer())
				continue
			} else {
				shards = protocol.ConvertToShardList(css.([]*protocol.ContractShards))
			}
			added := n.syncCl.AddPeer(conn.RemotePeer(), shards)
			if !added {
				conn.Close()
			}
		}
		n.syncSrv = protocol.NewSyncServer(rollupCfg, storageManager, m)

		blobByRangeHandler := protocol.MakeStreamHandler(resourcesCtx, log.New("serve", "blobs_by_range"), n.syncSrv.HandleGetBlobsByRangeRequest)
		n.host.SetStreamHandler(protocol.GetProtocolID(protocol.RequestBlobsByRangeProtocolID, rollupCfg.L2ChainID), blobByRangeHandler)
		blobByListHandler := protocol.MakeStreamHandler(resourcesCtx, log.New("serve", "blobs_by_list"), n.syncSrv.HandleGetBlobsByListRequest)
		n.host.SetStreamHandler(protocol.GetProtocolID(protocol.RequestBlobsByListProtocolID, rollupCfg.L2ChainID), blobByListHandler)

		// notify of any new connections/streams/etc.
		// TODO: use metric
		n.host.Network().Notify(NewNetworkNotifier(log, nil))
		// note: the IDDelta functionality was removed from libP2P, and no longer needs to be explicitly disabled.
		n.gs, err = NewGossipSub(resourcesCtx, n.host, n.gater, rollupCfg, setup, metrics, log)
		if err != nil {
			return fmt.Errorf("failed to start gossipsub router: %w", err)
		}

		log.Info("Started p2p host", "addrs", n.host.Addrs(), "peerID", n.host.ID().Pretty(), "targetPeers", setup.TargetPeers())

		tcpPort, err := FindActiveTCPPort(n.host)
		if err != nil {
			log.Warn("failed to find what TCP port p2p is binded to", "err", err)
		}

		// All nil if disabled.
		n.dv5Local, n.dv5Udp, err = setup.Discovery(log.New("p2p", "discv5"), l1ChainID, tcpPort)
		if err != nil {
			return fmt.Errorf("failed to start discv5: %w", err)
		}

		if metrics != nil {
			// go metrics.RecordBandwidth(resourcesCtx, bwc)
		}
	}
	return nil
}

func (n *NodeP2P) RequestL2Range(ctx context.Context, start, end uint64) (uint64, error) {
	return n.syncCl.RequestL2Range(ctx, start, end)
}

func (n *NodeP2P) Host() host.Host {
	return n.host
}

func (n *NodeP2P) Dv5Local() *enode.LocalNode {
	return n.dv5Local
}

func (n *NodeP2P) Dv5Udp() *discover.UDPv5 {
	return n.dv5Udp
}

func (n *NodeP2P) ConnectionManager() connmgr.ConnManager {
	return n.connMgr
}

func (n *NodeP2P) Start() {
	n.syncCl.Start()
}

func (n *NodeP2P) Close() error {
	var result *multierror.Error
	if n.dv5Udp != nil {
		n.dv5Udp.Close()
	}
	// if n.gsOut != nil {
	// 	if err := n.gsOut.Close(); err != nil {
	// 		result = multierror.Append(result, fmt.Errorf("failed to close gossip cleanly: %w", err))
	// 	}
	// }
	if n.host != nil {
		if err := n.host.Close(); err != nil {
			result = multierror.Append(result, fmt.Errorf("failed to close p2p host cleanly: %w", err))
		}
		if n.syncCl != nil {
			if err := n.syncCl.Close(); err != nil {
				result = multierror.Append(result, fmt.Errorf("failed to close p2p sync client cleanly: %w", err))
			}
		}
	}
	return result.ErrorOrNil()
}

func FindActiveTCPPort(h host.Host) (uint16, error) {
	var tcpPort uint16
	for _, addr := range h.Addrs() {
		tcpPortStr, err := addr.ValueForProtocol(ma.P_TCP)
		if err != nil {
			continue
		}
		v, err := strconv.ParseUint(tcpPortStr, 10, 16)
		if err != nil {
			continue
		}
		tcpPort = uint16(v)
		break
	}
	return tcpPort, nil
}
