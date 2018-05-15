package spvchain

import (
	"errors"
	"fmt"
	"net"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/btcsuite/btcd/addrmgr"
	"github.com/btcsuite/btcd/blockchain"
	"github.com/btcsuite/btcd/chaincfg"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/connmgr"
	"github.com/btcsuite/btcd/peer"
	"github.com/btcsuite/btcd/wire"
	"github.com/btcsuite/btcutil"
	"github.com/btcsuite/btcutil/gcs"
	"github.com/btcsuite/btcwallet/waddrmgr"
	"github.com/btcsuite/btcwallet/wallet"
	"github.com/btcsuite/btcwallet/walletdb"
)

// These are exported variables so they can be changed by users.
// TODO: Export functional options for these as much as possible so they can be
// changed call-to-call.
var (
	// ConnectionRetryInterval is the base amount of time to wait in between
	// retries when connecting to persistent peers.  It is adjusted by the
	// number of retries such that there is a retry backoff.
	ConnectionRetryInterval = time.Second * 5

	// UserAgentName is the user agent name and is used to help identify
	// ourselves to other bitcoin peers.
	UserAgentName = "spvchain"

	// UserAgentVersion is the user agent version and is used to help
	// identify ourselves to other bitcoin peers.
	UserAgentVersion = "0.0.1-alpha"

	// Services describes the services that are supported by the server.
	Services = wire.SFNodeCF

	// RequiredServices describes the services that are required to be
	// supported by outbound peers.
	RequiredServices = wire.SFNodeNetwork | wire.SFNodeCF

	// BanThreshold is the maximum ban score before a peer is banned.
	BanThreshold = uint32(100)

	// BanDuration is the duration of a ban.
	BanDuration = time.Hour * 24

	// TargetOutbound is the number of outbound peers to target.
	TargetOutbound = 8

	// MaxPeers is the maximum number of connections the client maintains.
	MaxPeers = 125

	// DisableDNSSeed disables getting initial addresses for Bitcoin nodes
	// from DNS.
	DisableDNSSeed = false

	// Timeout specifies how long to wait for a peer to answer a query.
	Timeout = time.Second * 5
)

// updatePeerHeightsMsg is a message sent from the blockmanager to the server
// after a new block has been accepted. The purpose of the message is to update
// the heights of peers that were known to announce the block before we
// connected it to the main chain or recognized it as an orphan. With these
// updates, peer heights will be kept up to date, allowing for fresh data when
// selecting sync peer candidacy.
type updatePeerHeightsMsg struct {
	newHash    *chainhash.Hash
	newHeight  int32
	originPeer *serverPeer
}

// peerState maintains state of inbound, persistent, outbound peers as well
// as banned peers and outbound groups.
type peerState struct {
	outboundPeers   map[int32]*serverPeer
	persistentPeers map[int32]*serverPeer
	banned          map[string]time.Time
	outboundGroups  map[string]int
}

// Count returns the count of all known peers.
func (ps *peerState) Count() int {
	return len(ps.outboundPeers) + len(ps.persistentPeers)
}

// forAllOutboundPeers is a helper function that runs closure on all outbound
// peers known to peerState.
func (ps *peerState) forAllOutboundPeers(closure func(sp *serverPeer)) {
	for _, e := range ps.outboundPeers {
		closure(e)
	}
	for _, e := range ps.persistentPeers {
		closure(e)
	}
}

// forAllPeers is a helper function that runs closure on all peers known to
// peerState.
func (ps *peerState) forAllPeers(closure func(sp *serverPeer)) {
	ps.forAllOutboundPeers(closure)
}

// Query options can be modified per-query, unlike global options.
// TODO: Make more query options that override global options.
type queryOptions struct {
	// queryTimeout lets the query know how long to wait for a peer to
	// answer the query before moving onto the next peer.
	queryTimeout time.Duration
}

// defaultQueryOptions returns a queryOptions set to package-level defaults.
func defaultQueryOptions() *queryOptions {
	return &queryOptions{
		queryTimeout: Timeout,
	}
}

// QueryTimeout is a query option that lets the query know to ask each peer we're
// connected to for its opinion, if any. By default, we only ask peers until one
// gives us a valid response.
func QueryTimeout(timeout time.Duration) func(*queryOptions) {
	return func(qo *queryOptions) {
		qo.queryTimeout = timeout
	}
}

type spMsg struct {
	sp  *serverPeer
	msg wire.Message
}

// queryPeers is a helper function that sends a query to one or more peers and
// waits for an answer. The timeout for queries is set by the QueryTimeout
// package-level variable.
func (ps *peerState) queryPeers(
	// selectPeer is a closure which decides whether or not to send the
	// query to the peer.
	selectPeer func(sp *serverPeer) bool,
	// queryMsg is the message to send to each peer selected by selectPeer.
	queryMsg wire.Message,
	// checkResponse is caled for every message within the timeout period.
	// The quit channel lets the query know to terminate because the
	// required response has been found. This is done by closing the
	// channel.
	checkResponse func(sp *serverPeer, resp wire.Message,
		quit chan<- struct{}),
	// options takes functional options for executing the query.
	options ...func(*queryOptions),
) {
	qo := defaultQueryOptions()
	for _, option := range options {
		option(qo)
	}
	// This will be shared state between the per-peer goroutines.
	quit := make(chan struct{})
	startQuery := make(chan struct{})
	var wg sync.WaitGroup
	channel := make(chan spMsg)

	// This goroutine will monitor all messages from all peers until the
	// peer goroutines all exit.
	go func() {
		for {
			select {
			case <-quit:
				close(channel)
				ps.forAllPeers(
					func(sp *serverPeer) {
						sp.unsubscribeRecvMsgs(channel)
					})
				return
			case sm := <-channel:
				// TODO: This will get stuck if checkResponse
				// gets stuck.
				checkResponse(sm.sp, sm.msg, quit)
			}
		}
	}()

	// Start a goroutine for each peer that potentially queries each peer
	ps.forAllPeers(func(sp *serverPeer) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if !selectPeer(sp) {
				return
			}
			timeout := make(<-chan time.Time)
			for {
				select {
				case <-timeout:
					// After timeout, we return and notify
					// another goroutine that we've done so.
					// We only send if there's someone left
					// to receive.
					startQuery <- struct{}{}
					return
				case <-quit:
					// After we're told to quit, we return.
					return
				case <-startQuery:
					// We're the lucky peer whose turn it is
					// to try to answer the current query.
					// TODO: Fix this to support multiple
					// queries at once. For now, we're
					// relying on the  query handling loop
					// to make sure we don't interrupt
					// another query. We need broadcast
					// support in OnRead to do this right.
					// TODO: Fix this to support either
					// querying *all* peers simultaneously
					// to avoid timeout delays, or starting
					// with the syncPeer when not querying
					// *all* peers.
					sp.subscribeRecvMsg(channel)
					sp.QueueMessage(queryMsg, nil)
					timeout = time.After(qo.queryTimeout)
				default:
				}
			}
		}()
	})
	startQuery <- struct{}{}
	wg.Wait()
	// If we timed out and didn't quit, make sure our response monitor
	// goroutine knows to quit.
	select {
	case <-quit:
	default:
		close(quit)
	}
}

// cfhRequest records which cfheaders we've requested, and the order in which
// we've requested them. Since there's no way to associate the cfheaders to the
// actual block hashes based on the cfheaders message to keep it compact, we
// track it this way.
type cfhRequest struct {
	extended bool
	stopHash chainhash.Hash
}

// cfRequest records which cfilters we've requested.
type cfRequest struct {
	extended  bool
	blockHash chainhash.Hash
}

// serverPeer extends the peer to maintain state shared by the server and
// the blockmanager.
type serverPeer struct {
	// The following variables must only be used atomically
	feeFilter int64

	*peer.Peer

	connReq            *connmgr.ConnReq
	server             *ChainService
	persistent         bool
	continueHash       *chainhash.Hash
	relayMtx           sync.Mutex
	requestQueue       []*wire.InvVect
	requestedCFHeaders map[cfhRequest]int
	knownAddresses     map[string]struct{}
	banScore           connmgr.DynamicBanScore
	quit               chan struct{}
	// The following slice of channels is used to subscribe to messages from
	// the peer. This allows broadcast to multiple subscribers at once,
	// allowing for multiple queries to be going to multiple peers at any
	// one time. The mutex is for subscribe/unsubscribe functionality.
	// The sends on these channels WILL NOT block; any messages the channel
	// can't accept will be dropped silently.
	recvSubscribers []chan<- spMsg
	mtxSubscribers  sync.RWMutex
}

// newServerPeer returns a new serverPeer instance. The peer needs to be set by
// the caller.
func newServerPeer(s *ChainService, isPersistent bool) *serverPeer {
	return &serverPeer{
		server:             s,
		persistent:         isPersistent,
		requestedCFHeaders: make(map[cfhRequest]int),
		knownAddresses:     make(map[string]struct{}),
		quit:               make(chan struct{}),
	}
}

// newestBlock returns the current best block hash and height using the format
// required by the configuration for the peer package.
func (sp *serverPeer) newestBlock() (*chainhash.Hash, int32, error) {
	best, err := sp.server.BestSnapshot()
	if err != nil {
		return nil, 0, err
	}
	return &best.Hash, best.Height, nil
}

// addKnownAddresses adds the given addresses to the set of known addresses to
// the peer to prevent sending duplicate addresses.
func (sp *serverPeer) addKnownAddresses(addresses []*wire.NetAddress) {
	for _, na := range addresses {
		sp.knownAddresses[addrmgr.NetAddressKey(na)] = struct{}{}
	}
}

// addressKnown true if the given address is already known to the peer.
func (sp *serverPeer) addressKnown(na *wire.NetAddress) bool {
	_, exists := sp.knownAddresses[addrmgr.NetAddressKey(na)]
	return exists
}

// addBanScore increases the persistent and decaying ban score fields by the
// values passed as parameters. If the resulting score exceeds half of the ban
// threshold, a warning is logged including the reason provided. Further, if
// the score is above the ban threshold, the peer will be banned and
// disconnected.
func (sp *serverPeer) addBanScore(persistent, transient uint32, reason string) {
	// No warning is logged and no score is calculated if banning is disabled.
	warnThreshold := BanThreshold >> 1
	if transient == 0 && persistent == 0 {
		// The score is not being increased, but a warning message is still
		// logged if the score is above the warn threshold.
		score := sp.banScore.Int()
		if score > warnThreshold {
			log.Warnf("Misbehaving peer %s: %s -- ban score is %d, "+
				"it was not increased this time", sp, reason, score)
		}
		return
	}
	score := sp.banScore.Increase(persistent, transient)
	if score > warnThreshold {
		log.Warnf("Misbehaving peer %s: %s -- ban score increased to %d",
			sp, reason, score)
		if score > BanThreshold {
			log.Warnf("Misbehaving peer %s -- banning and disconnecting",
				sp)
			sp.server.BanPeer(sp)
			sp.Disconnect()
		}
	}
}

// pushGetCFHeadersMsg sends a getcfheaders message for the provided block
// locator and stop hash to the connected peer.
func (sp *serverPeer) pushGetCFHeadersMsg(locator blockchain.BlockLocator,
	stopHash *chainhash.Hash, ext bool) error {
	msg := wire.NewMsgGetCFHeaders()
	msg.HashStop = *stopHash
	for _, hash := range locator {
		err := msg.AddBlockLocatorHash(hash)
		if err != nil {
			return err
		}
	}
	msg.Extended = ext
	sp.QueueMessage(msg, nil)
	return nil
}

// pushSendHeadersMsg sends a sendheaders message to the connected peer.
func (sp *serverPeer) pushSendHeadersMsg() error {
	if sp.VersionKnown() {
		if sp.ProtocolVersion() > wire.SendHeadersVersion {
			sp.QueueMessage(wire.NewMsgSendHeaders(), nil)
		}
	}
	return nil
}

// OnVerAck is invoked when a peer receives a verack bitcoin message and is used
// to send the "sendheaders" command to peers that are of a sufficienty new
// protocol version.
func (sp *serverPeer) OnVerAck(_ *peer.Peer, msg *wire.MsgVerAck) {
	sp.pushSendHeadersMsg()
}

// OnVersion is invoked when a peer receives a version bitcoin message
// and is used to negotiate the protocol version details as well as kick start
// the communications.
func (sp *serverPeer) OnVersion(_ *peer.Peer, msg *wire.MsgVersion) {
	// Add the remote peer time as a sample for creating an offset against
	// the local clock to keep the network time in sync.
	sp.server.timeSource.AddTimeSample(sp.Addr(), msg.Timestamp)

	// Signal the block manager this peer is a new sync candidate.
	sp.server.blockManager.NewPeer(sp)

	// Update the address manager and request known addresses from the
	// remote peer for outbound connections.  This is skipped when running
	// on the simulation test network since it is only intended to connect
	// to specified peers and actively avoids advertising and connecting to
	// discovered peers.
	if sp.server.chainParams.Net != chaincfg.SimNetParams.Net {
		addrManager := sp.server.addrManager
		// Request known addresses if the server address manager needs
		// more and the peer has a protocol version new enough to
		// include a timestamp with addresses.
		hasTimestamp := sp.ProtocolVersion() >=
			wire.NetAddressTimeVersion
		if addrManager.NeedMoreAddresses() && hasTimestamp {
			sp.QueueMessage(wire.NewMsgGetAddr(), nil)
		}

		// Mark the address as a known good address.
		addrManager.Good(sp.NA())
	}

	// Add valid peer to the server.
	sp.server.AddPeer(sp)
}

// OnInv is invoked when a peer receives an inv bitcoin message and is
// used to examine the inventory being advertised by the remote peer and react
// accordingly.  We pass the message down to blockmanager which will call
// QueueMessage with any appropriate responses.
func (sp *serverPeer) OnInv(p *peer.Peer, msg *wire.MsgInv) {
	log.Tracef("Got inv with %d items from %s", len(msg.InvList), p.Addr())
	newInv := wire.NewMsgInvSizeHint(uint(len(msg.InvList)))
	for _, invVect := range msg.InvList {
		if invVect.Type == wire.InvTypeTx {
			log.Tracef("Ignoring tx %s in inv from %v -- "+
				"SPV mode", invVect.Hash, sp)
			if sp.ProtocolVersion() >= wire.BIP0037Version {
				log.Infof("Peer %v is announcing "+
					"transactions -- disconnecting", sp)
				sp.Disconnect()
				return
			}
			continue
		}
		err := newInv.AddInvVect(invVect)
		if err != nil {
			log.Errorf("Failed to add inventory vector: %s", err)
			break
		}
	}

	if len(newInv.InvList) > 0 {
		sp.server.blockManager.QueueInv(newInv, sp)
	}
}

// OnHeaders is invoked when a peer receives a headers bitcoin
// message.  The message is passed down to the block manager.
func (sp *serverPeer) OnHeaders(p *peer.Peer, msg *wire.MsgHeaders) {
	log.Tracef("Got headers with %d items from %s", len(msg.Headers),
		p.Addr())
	sp.server.blockManager.QueueHeaders(msg, sp)
}

// handleGetData is invoked when a peer receives a getdata bitcoin message and
// is used to deliver block and transaction information.
func (sp *serverPeer) OnGetData(_ *peer.Peer, msg *wire.MsgGetData) {
	numAdded := 0
	notFound := wire.NewMsgNotFound()

	length := len(msg.InvList)
	// A decaying ban score increase is applied to prevent exhausting resources
	// with unusually large inventory queries.
	// Requesting more than the maximum inventory vector length within a short
	// period of time yields a score above the default ban threshold. Sustained
	// bursts of small requests are not penalized as that would potentially ban
	// peers performing IBD.
	// This incremental score decays each minute to half of its value.
	sp.addBanScore(0, uint32(length)*99/wire.MaxInvPerMsg, "getdata")

	// We wait on this wait channel periodically to prevent queuing
	// far more data than we can send in a reasonable time, wasting memory.
	// The waiting occurs after the database fetch for the next one to
	// provide a little pipelining.
	var waitChan chan struct{}
	doneChan := make(chan struct{}, 1)

	for i, iv := range msg.InvList {
		var c chan struct{}
		// If this will be the last message we send.
		if i == length-1 && len(notFound.InvList) == 0 {
			c = doneChan
		} else if (i+1)%3 == 0 {
			// Buffered so as to not make the send goroutine block.
			c = make(chan struct{}, 1)
		}
		var err error
		switch iv.Type {
		case wire.InvTypeTx:
			err = sp.server.pushTxMsg(sp, &iv.Hash, c, waitChan)
		default:
			log.Warnf("Unsupported type in inventory request %d",
				iv.Type)
			continue
		}
		if err != nil {
			notFound.AddInvVect(iv)

			// When there is a failure fetching the final entry
			// and the done channel was sent in due to there
			// being no outstanding not found inventory, consume
			// it here because there is now not found inventory
			// that will use the channel momentarily.
			if i == len(msg.InvList)-1 && c != nil {
				<-c
			}
		}
		numAdded++
		waitChan = c
	}
	if len(notFound.InvList) != 0 {
		sp.QueueMessage(notFound, doneChan)
	}

	// Wait for messages to be sent. We can send quite a lot of data at this
	// point and this will keep the peer busy for a decent amount of time.
	// We don't process anything else by them in this time so that we
	// have an idea of when we should hear back from them - else the idle
	// timeout could fire when we were only half done sending the blocks.
	if numAdded > 0 {
		<-doneChan
	}
}

// OnFeeFilter is invoked when a peer receives a feefilter bitcoin message and
// is used by remote peers to request that no transactions which have a fee rate
// lower than provided value are inventoried to them.  The peer will be
// disconnected if an invalid fee filter value is provided.
func (sp *serverPeer) OnFeeFilter(_ *peer.Peer, msg *wire.MsgFeeFilter) {
	// Check that the passed minimum fee is a valid amount.
	if msg.MinFee < 0 || msg.MinFee > btcutil.MaxSatoshi {
		log.Debugf("Peer %v sent an invalid feefilter '%v' -- "+
			"disconnecting", sp, btcutil.Amount(msg.MinFee))
		sp.Disconnect()
		return
	}

	atomic.StoreInt64(&sp.feeFilter, msg.MinFee)
}

// OnReject is invoked when a peer receives a reject bitcoin message and is
// used to notify the server about a rejected transaction.
func (sp *serverPeer) OnReject(_ *peer.Peer, msg *wire.MsgReject) {

}

// OnCFHeaders is invoked when a peer receives a cfheaders bitcoin message and
// is used to notify the server about a list of committed filter headers.
func (sp *serverPeer) OnCFHeaders(p *peer.Peer, msg *wire.MsgCFHeaders) {
	log.Tracef("Got cfheaders message with %d items from %s",
		len(msg.HeaderHashes), p.Addr())
	sp.server.blockManager.QueueCFHeaders(msg, sp)
}

// OnAddr is invoked when a peer receives an addr bitcoin message and is
// used to notify the server about advertised addresses.
func (sp *serverPeer) OnAddr(_ *peer.Peer, msg *wire.MsgAddr) {
	// Ignore addresses when running on the simulation test network.  This
	// helps prevent the network from becoming another public test network
	// since it will not be able to learn about other peers that have not
	// specifically been provided.
	if sp.server.chainParams.Net == chaincfg.SimNetParams.Net {
		return
	}

	// Ignore old style addresses which don't include a timestamp.
	if sp.ProtocolVersion() < wire.NetAddressTimeVersion {
		return
	}

	// A message that has no addresses is invalid.
	if len(msg.AddrList) == 0 {
		log.Errorf("Command [%s] from %s does not contain any addresses",
			msg.Command(), sp)
		sp.Disconnect()
		return
	}

	for _, na := range msg.AddrList {
		// Don't add more address if we're disconnecting.
		if !sp.Connected() {
			return
		}

		// Set the timestamp to 5 days ago if it's more than 24 hours
		// in the future so this address is one of the first to be
		// removed when space is needed.
		now := time.Now()
		if na.Timestamp.After(now.Add(time.Minute * 10)) {
			na.Timestamp = now.Add(-1 * time.Hour * 24 * 5)
		}

		// Add address to known addresses for this peer.
		sp.addKnownAddresses([]*wire.NetAddress{na})
	}

	// Add addresses to server address manager.  The address manager handles
	// the details of things such as preventing duplicate addresses, max
	// addresses, and last seen updates.
	// XXX bitcoind gives a 2 hour time penalty here, do we want to do the
	// same?
	sp.server.addrManager.AddAddresses(msg.AddrList, sp.NA())
}

// OnRead is invoked when a peer receives a message and it is used to update
// the bytes received by the server.
func (sp *serverPeer) OnRead(_ *peer.Peer, bytesRead int, msg wire.Message,
	err error) {
	sp.server.AddBytesReceived(uint64(bytesRead))
	// Try to send a message to the subscriber channel if it isn't nil, but
	// don't block on failure.
	sp.mtxSubscribers.RLock()
	defer sp.mtxSubscribers.RUnlock()
	for _, channel := range sp.recvSubscribers {
		if channel != nil {
			select {
			case channel <- spMsg{
				sp:  sp,
				msg: msg,
			}:
			default:
			}
		}
	}
}

// subscribeRecvMsg handles adding OnRead subscriptions to the server peer.
func (sp *serverPeer) subscribeRecvMsg(channel chan<- spMsg) {
	sp.mtxSubscribers.Lock()
	defer sp.mtxSubscribers.Unlock()
	sp.recvSubscribers = append(sp.recvSubscribers, channel)
}

// unsubscribeRecvMsgs handles removing OnRead subscriptions from the server
// peer.
func (sp *serverPeer) unsubscribeRecvMsgs(channel chan<- spMsg) {
	sp.mtxSubscribers.Lock()
	defer sp.mtxSubscribers.Unlock()
	var updatedSubscribers []chan<- spMsg
	for _, candidate := range sp.recvSubscribers {
		if candidate != channel {
			updatedSubscribers = append(updatedSubscribers,
				candidate)
		}
	}
	sp.recvSubscribers = updatedSubscribers
}

// OnWrite is invoked when a peer sends a message and it is used to update
// the bytes sent by the server.
func (sp *serverPeer) OnWrite(_ *peer.Peer, bytesWritten int, msg wire.Message, err error) {
	sp.server.AddBytesSent(uint64(bytesWritten))
}

// ChainService is instantiated with functional options
type ChainService struct {
	// The following variables must only be used atomically.
	// Putting the uint64s first makes them 64-bit aligned for 32-bit systems.
	bytesReceived uint64 // Total bytes received from all peers since start.
	bytesSent     uint64 // Total bytes sent by all peers since start.
	started       int32
	shutdown      int32

	namespace         walletdb.Namespace
	chainParams       chaincfg.Params
	addrManager       *addrmgr.AddrManager
	connManager       *connmgr.ConnManager
	blockManager      *blockManager
	newPeers          chan *serverPeer
	donePeers         chan *serverPeer
	banPeers          chan *serverPeer
	query             chan interface{}
	peerHeightsUpdate chan updatePeerHeightsMsg
	wg                sync.WaitGroup
	quit              chan struct{}
	timeSource        blockchain.MedianTimeSource
	services          wire.ServiceFlag

	cfilterRequests  map[cfRequest][]chan *gcs.Filter
	cfRequestHeaders map[cfRequest][2]*chainhash.Hash

	userAgentName    string
	userAgentVersion string
}

// BanPeer bans a peer that has already been connected to the server by ip.
func (s *ChainService) BanPeer(sp *serverPeer) {
	s.banPeers <- sp
}

// BestSnapshot returns the best block hash and height known to the database.
func (s *ChainService) BestSnapshot() (*waddrmgr.BlockStamp, error) {
	var best *waddrmgr.BlockStamp
	var err error
	err = s.namespace.View(func(tx walletdb.Tx) error {
		best, err = syncedTo(tx)
		return err
	})
	if err != nil {
		return nil, err
	}
	return best, nil
}

// LatestBlockLocator returns the block locator for the latest known block
// stored in the database.
func (s *ChainService) LatestBlockLocator() (blockchain.BlockLocator, error) {
	var locator blockchain.BlockLocator
	var err error
	err = s.namespace.View(func(tx walletdb.Tx) error {
		best, err := syncedTo(tx)
		if err != nil {
			return err
		}
		locator = blockLocatorFromHash(tx, best.Hash)
		return nil
	})
	if err != nil {
		return nil, err
	}
	return locator, nil
}

// AddPeer adds a new peer that has already been connected to the server.
func (s *ChainService) AddPeer(sp *serverPeer) {
	s.newPeers <- sp
}

// AddBytesSent adds the passed number of bytes to the total bytes sent counter
// for the server.  It is safe for concurrent access.
func (s *ChainService) AddBytesSent(bytesSent uint64) {
	atomic.AddUint64(&s.bytesSent, bytesSent)
}

// AddBytesReceived adds the passed number of bytes to the total bytes received
// counter for the server.  It is safe for concurrent access.
func (s *ChainService) AddBytesReceived(bytesReceived uint64) {
	atomic.AddUint64(&s.bytesReceived, bytesReceived)
}

// NetTotals returns the sum of all bytes received and sent across the network
// for all peers.  It is safe for concurrent access.
func (s *ChainService) NetTotals() (uint64, uint64) {
	return atomic.LoadUint64(&s.bytesReceived),
		atomic.LoadUint64(&s.bytesSent)
}

// pushTxMsg sends a tx message for the provided transaction hash to the
// connected peer.  An error is returned if the transaction hash is not known.
func (s *ChainService) pushTxMsg(sp *serverPeer, hash *chainhash.Hash, doneChan chan<- struct{}, waitChan <-chan struct{}) error {
	// Attempt to fetch the requested transaction from the pool.  A
	// call could be made to check for existence first, but simply trying
	// to fetch a missing transaction results in the same behavior.
	/*	tx, err := s.txMemPool.FetchTransaction(hash)
		if err != nil {
			log.Tracef("Unable to fetch tx %v from transaction "+
				"pool: %v", hash, err)

			if doneChan != nil {
				doneChan <- struct{}{}
			}
			return err
		}

		// Once we have fetched data wait for any previous operation to finish.
		if waitChan != nil {
			<-waitChan
		}

		sp.QueueMessage(tx.MsgTx(), doneChan) */

	return nil
}

// peerHandler is used to handle peer operations such as adding and removing
// peers to and from the server, banning peers, and broadcasting messages to
// peers.  It must be run in a goroutine.
func (s *ChainService) peerHandler() {
	// Start the address manager and block manager, both of which are needed
	// by peers.  This is done here since their lifecycle is closely tied
	// to this handler and rather than adding more channels to sychronize
	// things, it's easier and slightly faster to simply start and stop them
	// in this handler.
	s.addrManager.Start()
	s.blockManager.Start()

	state := &peerState{
		persistentPeers: make(map[int32]*serverPeer),
		outboundPeers:   make(map[int32]*serverPeer),
		banned:          make(map[string]time.Time),
		outboundGroups:  make(map[string]int),
	}

	if !DisableDNSSeed {
		// Add peers discovered through DNS to the address manager.
		connmgr.SeedFromDNS(&s.chainParams, RequiredServices,
			net.LookupIP, func(addrs []*wire.NetAddress) {
				// Bitcoind uses a lookup of the dns seeder here. This
				// is rather strange since the values looked up by the
				// DNS seed lookups will vary quite a lot.
				// to replicate this behaviour we put all addresses as
				// having come from the first one.
				s.addrManager.AddAddresses(addrs, addrs[0])
			})
	}
	go s.connManager.Start()

out:
	for {
		select {
		// New peers connected to the server.
		case p := <-s.newPeers:
			s.handleAddPeerMsg(state, p)

		// Disconnected peers.
		case p := <-s.donePeers:
			s.handleDonePeerMsg(state, p)

		// Block accepted in mainchain or orphan, update peer height.
		case umsg := <-s.peerHeightsUpdate:
			s.handleUpdatePeerHeights(state, umsg)

		// Peer to ban.
		case p := <-s.banPeers:
			s.handleBanPeerMsg(state, p)

		case qmsg := <-s.query:
			s.handleQuery(state, qmsg)

		case <-s.quit:
			// Disconnect all peers on server shutdown.
			state.forAllPeers(func(sp *serverPeer) {
				log.Tracef("Shutdown peer %s", sp)
				sp.Disconnect()
			})
			break out
		}
	}

	s.connManager.Stop()
	s.blockManager.Stop()
	s.addrManager.Stop()

	// Drain channels before exiting so nothing is left waiting around
	// to send.
cleanup:
	for {
		select {
		case <-s.newPeers:
		case <-s.donePeers:
		case <-s.peerHeightsUpdate:
		case <-s.query:
		default:
			break cleanup
		}
	}
	s.wg.Done()
	log.Tracef("Peer handler done")
}

// Config is a struct detailing the configuration of the chain service.
type Config struct {
	DataDir      string
	Namespace    walletdb.Namespace
	ChainParams  chaincfg.Params
	ConnectPeers []string
	AddPeers     []string
}

// NewChainService returns a new chain service configured to connect to the
// bitcoin network type specified by chainParams.  Use start to begin syncing
// with peers.
func NewChainService(cfg Config) (*ChainService, error) {
	amgr := addrmgr.New(cfg.DataDir, net.LookupIP)

	s := ChainService{
		chainParams:       cfg.ChainParams,
		addrManager:       amgr,
		newPeers:          make(chan *serverPeer, MaxPeers),
		donePeers:         make(chan *serverPeer, MaxPeers),
		banPeers:          make(chan *serverPeer, MaxPeers),
		query:             make(chan interface{}),
		quit:              make(chan struct{}),
		peerHeightsUpdate: make(chan updatePeerHeightsMsg),
		namespace:         cfg.Namespace,
		timeSource:        blockchain.NewMedianTime(),
		services:          Services,
		userAgentName:     UserAgentName,
		userAgentVersion:  UserAgentVersion,
		cfilterRequests:   make(map[cfRequest][]chan *gcs.Filter),
		cfRequestHeaders:  make(map[cfRequest][2]*chainhash.Hash),
	}

	err := createSPVNS(s.namespace, &s.chainParams)
	if err != nil {
		return nil, err
	}

	bm, err := newBlockManager(&s)
	if err != nil {
		return nil, err
	}
	s.blockManager = bm

	// Only setup a function to return new addresses to connect to when
	// not running in connect-only mode.  The simulation network is always
	// in connect-only mode since it is only intended to connect to
	// specified peers and actively avoid advertising and connecting to
	// discovered peers in order to prevent it from becoming a public test
	// network.
	var newAddressFunc func() (net.Addr, error)
	if s.chainParams.Net != chaincfg.SimNetParams.Net {
		newAddressFunc = func() (net.Addr, error) {
			for tries := 0; tries < 100; tries++ {
				addr := s.addrManager.GetAddress()
				if addr == nil {
					break
				}

				// Address will not be invalid, local or unroutable
				// because addrmanager rejects those on addition.
				// Just check that we don't already have an address
				// in the same group so that we are not connecting
				// to the same network segment at the expense of
				// others.
				key := addrmgr.GroupKey(addr.NetAddress())
				if s.OutboundGroupCount(key) != 0 {
					continue
				}

				// only allow recent nodes (10mins) after we failed 30
				// times
				if tries < 30 && time.Since(addr.LastAttempt()) < 10*time.Minute {
					continue
				}

				// allow nondefault ports after 50 failed tries.
				if tries < 50 && fmt.Sprintf("%d", addr.NetAddress().Port) !=
					s.chainParams.DefaultPort {
					continue
				}

				addrString := addrmgr.NetAddressKey(addr.NetAddress())
				return addrStringToNetAddr(addrString)
			}

			return nil, errors.New("no valid connect address")
		}
	}

	// Create a connection manager.
	if MaxPeers < TargetOutbound {
		TargetOutbound = MaxPeers
	}
	cmgr, err := connmgr.New(&connmgr.Config{
		RetryDuration:  ConnectionRetryInterval,
		TargetOutbound: uint32(TargetOutbound),
		Dial: func(addr net.Addr) (net.Conn, error) {
			return net.Dial(addr.Network(), addr.String())
		},
		OnConnection:  s.outboundPeerConnected,
		GetNewAddress: newAddressFunc,
	})
	if err != nil {
		return nil, err
	}
	s.connManager = cmgr

	// Start up persistent peers.
	permanentPeers := cfg.ConnectPeers
	if len(permanentPeers) == 0 {
		permanentPeers = cfg.AddPeers
	}
	for _, addr := range permanentPeers {
		tcpAddr, err := addrStringToNetAddr(addr)
		if err != nil {
			return nil, err
		}

		go s.connManager.Connect(&connmgr.ConnReq{
			Addr:      tcpAddr,
			Permanent: true,
		})
	}

	return &s, nil
}

// addrStringToNetAddr takes an address in the form of 'host:port' and returns
// a net.Addr which maps to the original address with any host names resolved
// to IP addresses.
func addrStringToNetAddr(addr string) (net.Addr, error) {
	host, strPort, err := net.SplitHostPort(addr)
	if err != nil {
		return nil, err
	}

	// Attempt to look up an IP address associated with the parsed host.
	ips, err := net.LookupIP(host)
	if err != nil {
		return nil, err
	}
	if len(ips) == 0 {
		return nil, fmt.Errorf("no addresses found for %s", host)
	}

	port, err := strconv.Atoi(strPort)
	if err != nil {
		return nil, err
	}

	return &net.TCPAddr{
		IP:   ips[0],
		Port: port,
	}, nil
}

// handleUpdatePeerHeight updates the heights of all peers who were known to
// announce a block we recently accepted.
func (s *ChainService) handleUpdatePeerHeights(state *peerState, umsg updatePeerHeightsMsg) {
	state.forAllPeers(func(sp *serverPeer) {
		// The origin peer should already have the updated height.
		if sp == umsg.originPeer {
			return
		}

		// This is a pointer to the underlying memory which doesn't
		// change.
		latestBlkHash := sp.LastAnnouncedBlock()

		// Skip this peer if it hasn't recently announced any new blocks.
		if latestBlkHash == nil {
			return
		}

		// If the peer has recently announced a block, and this block
		// matches our newly accepted block, then update their block
		// height.
		if *latestBlkHash == *umsg.newHash {
			sp.UpdateLastBlockHeight(umsg.newHeight)
			sp.UpdateLastAnnouncedBlock(nil)
		}
	})
}

// handleAddPeerMsg deals with adding new peers.  It is invoked from the
// peerHandler goroutine.
func (s *ChainService) handleAddPeerMsg(state *peerState, sp *serverPeer) bool {
	if sp == nil {
		return false
	}

	// Ignore new peers if we're shutting down.
	if atomic.LoadInt32(&s.shutdown) != 0 {
		log.Infof("New peer %s ignored - server is shutting down", sp)
		sp.Disconnect()
		return false
	}

	// Disconnect banned peers.
	host, _, err := net.SplitHostPort(sp.Addr())
	if err != nil {
		log.Debugf("can't split host/port: %s", err)
		sp.Disconnect()
		return false
	}
	if banEnd, ok := state.banned[host]; ok {
		if time.Now().Before(banEnd) {
			log.Debugf("Peer %s is banned for another %v - disconnecting",
				host, banEnd.Sub(time.Now()))
			sp.Disconnect()
			return false
		}

		log.Infof("Peer %s is no longer banned", host)
		delete(state.banned, host)
	}

	// TODO: Check for max peers from a single IP.

	// Limit max number of total peers.
	if state.Count() >= MaxPeers {
		log.Infof("Max peers reached [%d] - disconnecting peer %s",
			MaxPeers, sp)
		sp.Disconnect()
		// TODO: how to handle permanent peers here?
		// they should be rescheduled.
		return false
	}

	// Add the new peer and start it.
	log.Debugf("New peer %s", sp)
	state.outboundGroups[addrmgr.GroupKey(sp.NA())]++
	if sp.persistent {
		state.persistentPeers[sp.ID()] = sp
	} else {
		state.outboundPeers[sp.ID()] = sp
	}

	return true
}

// handleDonePeerMsg deals with peers that have signalled they are done.  It is
// invoked from the peerHandler goroutine.
func (s *ChainService) handleDonePeerMsg(state *peerState, sp *serverPeer) {
	var list map[int32]*serverPeer
	if sp.persistent {
		list = state.persistentPeers
	} else {
		list = state.outboundPeers
	}
	if _, ok := list[sp.ID()]; ok {
		if !sp.Inbound() && sp.VersionKnown() {
			state.outboundGroups[addrmgr.GroupKey(sp.NA())]--
		}
		if !sp.Inbound() && sp.connReq != nil {
			s.connManager.Disconnect(sp.connReq.ID())
		}
		delete(list, sp.ID())
		log.Debugf("Removed peer %s", sp)
		return
	}

	if sp.connReq != nil {
		s.connManager.Disconnect(sp.connReq.ID())
	}

	// Update the address' last seen time if the peer has acknowledged
	// our version and has sent us its version as well.
	if sp.VerAckReceived() && sp.VersionKnown() && sp.NA() != nil {
		s.addrManager.Connected(sp.NA())
	}

	// If we get here it means that either we didn't know about the peer
	// or we purposefully deleted it.
}

// handleBanPeerMsg deals with banning peers.  It is invoked from the
// peerHandler goroutine.
func (s *ChainService) handleBanPeerMsg(state *peerState, sp *serverPeer) {
	host, _, err := net.SplitHostPort(sp.Addr())
	if err != nil {
		log.Debugf("can't split ban peer %s: %s", sp.Addr(), err)
		return
	}
	log.Infof("Banned peer %s for %v", host, BanDuration)
	state.banned[host] = time.Now().Add(BanDuration)
}

// disconnectPeer attempts to drop the connection of a tageted peer in the
// passed peer list. Targets are identified via usage of the passed
// `compareFunc`, which should return `true` if the passed peer is the target
// peer. This function returns true on success and false if the peer is unable
// to be located. If the peer is found, and the passed callback: `whenFound'
// isn't nil, we call it with the peer as the argument before it is removed
// from the peerList, and is disconnected from the server.
func disconnectPeer(peerList map[int32]*serverPeer, compareFunc func(*serverPeer) bool, whenFound func(*serverPeer)) bool {
	for addr, peer := range peerList {
		if compareFunc(peer) {
			if whenFound != nil {
				whenFound(peer)
			}

			// This is ok because we are not continuing
			// to iterate so won't corrupt the loop.
			delete(peerList, addr)
			peer.Disconnect()
			return true
		}
	}
	return false
}

// sendUnminedTxs iterates through all transactions that spend from wallet
// credits that are not known to have been mined into a block, and attempts to
// send each to the chain server for relay.
//
// TODO: This should return an error if any of these lookups or sends fail, but
// since send errors due to double spends need to be handled gracefully and this
// isn't done yet, all sending errors are simply logged.
func (s *ChainService) sendUnminedTxs(w *wallet.Wallet) error {
	/*txs, err := w.TxStore.UnminedTxs()
	if err != nil {
		return err
	}
	rpcClient := s.rpcClient
	for _, tx := range txs {
		resp, err := rpcClient.SendRawTransaction(tx, false)
		if err != nil {
			// TODO(jrick): Check error for if this tx is a double spend,
			// remove it if so.
			log.Debugf("Could not resend transaction %v: %v",
				tx.TxHash(), err)
			continue
		}
		log.Debugf("Resent unmined transaction %v", resp)
	}*/
	return nil
}

// PublishTransaction sends the transaction to the consensus RPC server so it
// can be propigated to other nodes and eventually mined.
func (s *ChainService) PublishTransaction(tx *wire.MsgTx) error {
	/*_, err := s.rpcClient.SendRawTransaction(tx, false)
	return err*/
	return nil
}

// AnnounceNewTransactions generates and relays inventory vectors and notifies
// both websocket and getblocktemplate long poll clients of the passed
// transactions.  This function should be called whenever new transactions
// are added to the mempool.
func (s *ChainService) AnnounceNewTransactions( /*newTxs []*mempool.TxDesc*/ ) {
	// Generate and relay inventory vectors for all newly accepted
	// transactions into the memory pool due to the original being
	// accepted.
	/*for _, txD := range newTxs {
		// Generate the inventory vector and relay it.
		iv := wire.NewInvVect(wire.InvTypeTx, txD.Tx.Hash())
		s.RelayInventory(iv, txD)

		if s.rpcServer != nil {
			// Notify websocket clients about mempool transactions.
			s.rpcServer.ntfnMgr.NotifyMempoolTx(txD.Tx, true)

			// Potentially notify any getblocktemplate long poll clients
			// about stale block templates due to the new transaction.
			s.rpcServer.gbtWorkState.NotifyMempoolTx(
				s.txMemPool.LastUpdated())
		}
	}*/
}

// newPeerConfig returns the configuration for the given serverPeer.
func newPeerConfig(sp *serverPeer) *peer.Config {
	return &peer.Config{
		Listeners: peer.MessageListeners{
			OnVersion: sp.OnVersion,
			//OnVerAck:    sp.OnVerAck, // Don't use sendheaders yet
			OnInv:       sp.OnInv,
			OnHeaders:   sp.OnHeaders,
			OnCFHeaders: sp.OnCFHeaders,
			OnGetData:   sp.OnGetData,
			OnReject:    sp.OnReject,
			OnFeeFilter: sp.OnFeeFilter,
			OnAddr:      sp.OnAddr,
			OnRead:      sp.OnRead,
			OnWrite:     sp.OnWrite,

			// Note: The reference client currently bans peers that send alerts
			// not signed with its key.  We could verify against their key, but
			// since the reference client is currently unwilling to support
			// other implementations' alert messages, we will not relay theirs.
			OnAlert: nil,
		},
		NewestBlock:      sp.newestBlock,
		HostToNetAddress: sp.server.addrManager.HostToNetAddress,
		UserAgentName:    sp.server.userAgentName,
		UserAgentVersion: sp.server.userAgentVersion,
		ChainParams:      &sp.server.chainParams,
		Services:         sp.server.services,
		ProtocolVersion:  wire.FeeFilterVersion,
		DisableRelayTx:   true,
	}
}

// outboundPeerConnected is invoked by the connection manager when a new
// outbound connection is established.  It initializes a new outbound server
// peer instance, associates it with the relevant state such as the connection
// request instance and the connection itself, and finally notifies the address
// manager of the attempt.
func (s *ChainService) outboundPeerConnected(c *connmgr.ConnReq, conn net.Conn) {
	sp := newServerPeer(s, c.Permanent)
	p, err := peer.NewOutboundPeer(newPeerConfig(sp), c.Addr.String())
	if err != nil {
		log.Debugf("Cannot create outbound peer %s: %s", c.Addr, err)
		s.connManager.Disconnect(c.ID())
	}
	sp.Peer = p
	sp.connReq = c
	sp.AssociateConnection(conn)
	go s.peerDoneHandler(sp)
	s.addrManager.Attempt(sp.NA())
}

// peerDoneHandler handles peer disconnects by notifiying the server that it's
// done along with other performing other desirable cleanup.
func (s *ChainService) peerDoneHandler(sp *serverPeer) {
	sp.WaitForDisconnect()
	s.donePeers <- sp

	// Only tell block manager we are gone if we ever told it we existed.
	if sp.VersionKnown() {
		s.blockManager.DonePeer(sp)
	}
	close(sp.quit)
}

// ConnectedCount returns the number of currently connected peers.
func (s *ChainService) ConnectedCount() int32 {
	replyChan := make(chan int32)

	s.query <- getConnCountMsg{reply: replyChan}

	return <-replyChan
}

// OutboundGroupCount returns the number of peers connected to the given
// outbound group key.
func (s *ChainService) OutboundGroupCount(key string) int {
	replyChan := make(chan int)
	s.query <- getOutboundGroup{key: key, reply: replyChan}
	return <-replyChan
}

// AddedNodeInfo returns an array of btcjson.GetAddedNodeInfoResult structures
// describing the persistent (added) nodes.
func (s *ChainService) AddedNodeInfo() []*serverPeer {
	replyChan := make(chan []*serverPeer)
	s.query <- getAddedNodesMsg{reply: replyChan}
	return <-replyChan
}

// Peers returns an array of all connected peers.
func (s *ChainService) Peers() []*serverPeer {
	replyChan := make(chan []*serverPeer)

	s.query <- getPeersMsg{reply: replyChan}

	return <-replyChan
}

// DisconnectNodeByAddr disconnects a peer by target address. Both outbound and
// inbound nodes will be searched for the target node. An error message will
// be returned if the peer was not found.
func (s *ChainService) DisconnectNodeByAddr(addr string) error {
	replyChan := make(chan error)

	s.query <- disconnectNodeMsg{
		cmp:   func(sp *serverPeer) bool { return sp.Addr() == addr },
		reply: replyChan,
	}

	return <-replyChan
}

// DisconnectNodeByID disconnects a peer by target node id. Both outbound and
// inbound nodes will be searched for the target node. An error message will be
// returned if the peer was not found.
func (s *ChainService) DisconnectNodeByID(id int32) error {
	replyChan := make(chan error)

	s.query <- disconnectNodeMsg{
		cmp:   func(sp *serverPeer) bool { return sp.ID() == id },
		reply: replyChan,
	}

	return <-replyChan
}

// RemoveNodeByAddr removes a peer from the list of persistent peers if
// present. An error will be returned if the peer was not found.
func (s *ChainService) RemoveNodeByAddr(addr string) error {
	replyChan := make(chan error)

	s.query <- removeNodeMsg{
		cmp:   func(sp *serverPeer) bool { return sp.Addr() == addr },
		reply: replyChan,
	}

	return <-replyChan
}

// RemoveNodeByID removes a peer by node ID from the list of persistent peers
// if present. An error will be returned if the peer was not found.
func (s *ChainService) RemoveNodeByID(id int32) error {
	replyChan := make(chan error)

	s.query <- removeNodeMsg{
		cmp:   func(sp *serverPeer) bool { return sp.ID() == id },
		reply: replyChan,
	}

	return <-replyChan
}

// ConnectNode adds `addr' as a new outbound peer. If permanent is true then the
// peer will be persistent and reconnect if the connection is lost.
// It is an error to call this with an already existing peer.
func (s *ChainService) ConnectNode(addr string, permanent bool) error {
	replyChan := make(chan error)

	s.query <- connectNodeMsg{addr: addr, permanent: permanent, reply: replyChan}

	return <-replyChan
}

// ForAllPeers runs a closure over all peers (outbound and persistent) to which
// the ChainService is connected. Nothing is returned because the peerState's
// ForAllPeers method doesn't return anything as the closure passed to it
// doesn't return anything.
func (s *ChainService) ForAllPeers(closure func(sp *serverPeer)) {
	s.query <- forAllPeersMsg{
		closure: closure,
	}
}

// UpdatePeerHeights updates the heights of all peers who have have announced
// the latest connected main chain block, or a recognized orphan. These height
// updates allow us to dynamically refresh peer heights, ensuring sync peer
// selection has access to the latest block heights for each peer.
func (s *ChainService) UpdatePeerHeights(latestBlkHash *chainhash.Hash, latestHeight int32, updateSource *serverPeer) {
	s.peerHeightsUpdate <- updatePeerHeightsMsg{
		newHash:    latestBlkHash,
		newHeight:  latestHeight,
		originPeer: updateSource,
	}
}

// rebroadcastHandler keeps track of user submitted inventories that we have
// sent out but have not yet made it into a block. We periodically rebroadcast
// them in case our peers restarted or otherwise lost track of them.
func (s *ChainService) rebroadcastHandler() {
	// Wait 5 min before first tx rebroadcast.
	timer := time.NewTimer(5 * time.Minute)
	//pendingInvs := make(map[wire.InvVect]interface{})

out:
	for {
		select {
		/*case riv := <-s.modifyRebroadcastInv:
		switch msg := riv.(type) {
		// Incoming InvVects are added to our map of RPC txs.
		case broadcastInventoryAdd:
			pendingInvs[*msg.invVect] = msg.data

		// When an InvVect has been added to a block, we can
		// now remove it, if it was present.
		case broadcastInventoryDel:
			if _, ok := pendingInvs[*msg]; ok {
				delete(pendingInvs, *msg)
			}
		}*/

		case <-timer.C: /*
				// Any inventory we have has not made it into a block
				// yet. We periodically resubmit them until they have.
				for iv, data := range pendingInvs {
					ivCopy := iv
					s.RelayInventory(&ivCopy, data)
				}

				// Process at a random time up to 30mins (in seconds)
				// in the future.
				timer.Reset(time.Second *
					time.Duration(randomUint16Number(1800))) */

		case <-s.quit:
			break out
		}
	}

	timer.Stop()

	// Drain channels before exiting so nothing is left waiting around
	// to send.
	/*cleanup:
	for {
		select {
		//case <-s.modifyRebroadcastInv:
		default:
			break cleanup
		}
	}*/
	s.wg.Done()
}

// Start begins connecting to peers and syncing the blockchain.
func (s *ChainService) Start() {
	// Already started?
	if atomic.AddInt32(&s.started, 1) != 1 {
		return
	}

	// Start the peer handler which in turn starts the address and block
	// managers.
	s.wg.Add(2)
	go s.peerHandler()
	go s.rebroadcastHandler()

}

// Stop gracefully shuts down the server by stopping and disconnecting all
// peers and the main listener.
func (s *ChainService) Stop() error {
	// Make sure this only happens once.
	if atomic.AddInt32(&s.shutdown, 1) != 1 {
		return nil
	}

	// Signal the remaining goroutines to quit.
	close(s.quit)
	s.wg.Wait()
	return nil
}

// GetBlockByHeight gets block information from the ChainService database by
// its height.
func (s *ChainService) GetBlockByHeight(height uint32) (wire.BlockHeader,
	uint32, error) {
	var bh wire.BlockHeader
	var h uint32
	var err error
	err = s.namespace.View(func(dbTx walletdb.Tx) error {
		bh, h, err = getBlockByHeight(dbTx, height)
		return err
	})
	return bh, h, err
}

// GetBlockByHash gets block information from the ChainService database by its
// hash.
func (s *ChainService) GetBlockByHash(hash chainhash.Hash) (wire.BlockHeader,
	uint32, error) {
	var bh wire.BlockHeader
	var h uint32
	var err error
	err = s.namespace.View(func(dbTx walletdb.Tx) error {
		bh, h, err = getBlockByHash(dbTx, hash)
		return err
	})
	return bh, h, err
}

// LatestBlock gets the latest block's information from the ChainService
// database.
func (s *ChainService) LatestBlock() (wire.BlockHeader, uint32, error) {
	var bh wire.BlockHeader
	var h uint32
	var err error
	err = s.namespace.View(func(dbTx walletdb.Tx) error {
		bh, h, err = latestBlock(dbTx)
		return err
	})
	return bh, h, err
}

// putBlock puts a verified block header and height in the ChainService
// database.
func (s *ChainService) putBlock(header wire.BlockHeader, height uint32) error {
	return s.namespace.Update(func(dbTx walletdb.Tx) error {
		return putBlock(dbTx, header, height)
	})
}

// putBasicHeader puts a verified basic filter header in the ChainService
// database.
func (s *ChainService) putBasicHeader(blockHash chainhash.Hash,
	filterTip chainhash.Hash) error {
	return s.namespace.Update(func(dbTx walletdb.Tx) error {
		return putBasicHeader(dbTx, blockHash, filterTip)
	})
}

// putExtHeader puts a verified extended filter header in the ChainService
// database.
func (s *ChainService) putExtHeader(blockHash chainhash.Hash,
	filterTip chainhash.Hash) error {
	return s.namespace.Update(func(dbTx walletdb.Tx) error {
		return putExtHeader(dbTx, blockHash, filterTip)
	})
}

// GetBasicHeader gets a verified basic filter header from the ChainService
// database.
func (s *ChainService) GetBasicHeader(blockHash chainhash.Hash) (*chainhash.Hash,
	error) {
	var filterTip *chainhash.Hash
	var err error
	err = s.namespace.View(func(dbTx walletdb.Tx) error {
		filterTip, err = getBasicHeader(dbTx, blockHash)
		return err
	})
	return filterTip, err
}

// GetExtHeader gets a verified extended filter header from the ChainService
// database.
func (s *ChainService) GetExtHeader(blockHash chainhash.Hash) (*chainhash.Hash,
	error) {
	var filterTip *chainhash.Hash
	var err error
	err = s.namespace.View(func(dbTx walletdb.Tx) error {
		filterTip, err = getExtHeader(dbTx, blockHash)
		return err
	})
	return filterTip, err
}

// putBasicFilter puts a verified basic filter in the ChainService database.
func (s *ChainService) putBasicFilter(blockHash chainhash.Hash,
	filter *gcs.Filter) error {
	return s.namespace.Update(func(dbTx walletdb.Tx) error {
		return putBasicFilter(dbTx, blockHash, filter)
	})
}

// putExtFilter puts a verified extended filter in the ChainService database.
func (s *ChainService) putExtFilter(blockHash chainhash.Hash,
	filter *gcs.Filter) error {
	return s.namespace.Update(func(dbTx walletdb.Tx) error {
		return putExtFilter(dbTx, blockHash, filter)
	})
}

// GetBasicFilter gets a verified basic filter from the ChainService database.
func (s *ChainService) GetBasicFilter(blockHash chainhash.Hash) (*gcs.Filter,
	error) {
	var filter *gcs.Filter
	var err error
	err = s.namespace.View(func(dbTx walletdb.Tx) error {
		filter, err = getBasicFilter(dbTx, blockHash)
		return err
	})
	return filter, err
}

// GetExtFilter gets a verified extended filter from the ChainService database.
func (s *ChainService) GetExtFilter(blockHash chainhash.Hash) (*gcs.Filter,
	error) {
	var filter *gcs.Filter
	var err error
	err = s.namespace.View(func(dbTx walletdb.Tx) error {
		filter, err = getExtFilter(dbTx, blockHash)
		return err
	})
	return filter, err
}

// putMaxBlockHeight puts the max block height to the ChainService database.
func (s *ChainService) putMaxBlockHeight(maxBlockHeight uint32) error {
	return s.namespace.Update(func(dbTx walletdb.Tx) error {
		return putMaxBlockHeight(dbTx, maxBlockHeight)
	})
}

func (s *ChainService) rollbackLastBlock() (*waddrmgr.BlockStamp, error) {
	var bs *waddrmgr.BlockStamp
	var err error
	err = s.namespace.Update(func(dbTx walletdb.Tx) error {
		bs, err = rollbackLastBlock(dbTx)
		return err
	})
	return bs, err
}

func (s *ChainService) rollbackToHeight(height uint32) (*waddrmgr.BlockStamp, error) {
	var bs *waddrmgr.BlockStamp
	var err error
	err = s.namespace.Update(func(dbTx walletdb.Tx) error {
		bs, err = syncedTo(dbTx)
		if err != nil {
			return err
		}
		for uint32(bs.Height) > height {
			bs, err = rollbackLastBlock(dbTx)
			if err != nil {
				return err
			}
		}
		return nil
	})
	return bs, err
}

// IsCurrent lets the caller know whether the chain service's block manager
// thinks its view of the network is current.
func (s *ChainService) IsCurrent() bool {
	return s.blockManager.IsCurrent()
}

// GetCFilter gets a cfilter from the database. Failing that, it requests the
// cfilter from the network and writes it to the database.
func (s *ChainService) GetCFilter(blockHash chainhash.Hash,
	extended bool) *gcs.Filter {
	getFilter := s.GetBasicFilter
	getHeader := s.GetBasicHeader
	putFilter := s.putBasicFilter
	if extended {
		getFilter = s.GetExtFilter
		getHeader = s.GetExtHeader
		putFilter = s.putExtFilter
	}
	filter, err := getFilter(blockHash)
	if err == nil && filter != nil {
		return filter
	}
	block, _, err := s.GetBlockByHash(blockHash)
	if err != nil || block.BlockHash() != blockHash {
		return nil
	}
	curHeader, err := getHeader(blockHash)
	if err != nil {
		return nil
	}
	prevHeader, err := getHeader(block.PrevBlock)
	if err != nil {
		return nil
	}
	replyChan := make(chan *gcs.Filter)
	s.query <- getCFilterMsg{
		cfRequest: cfRequest{
			blockHash: blockHash,
			extended:  extended,
		},
		prevHeader: prevHeader,
		curHeader:  curHeader,
		reply:      replyChan,
	}
	filter = <-replyChan
	if filter != nil {
		putFilter(blockHash, filter)
		log.Tracef("Wrote filter for block %s, extended: %t",
			blockHash, extended)
	}
	return filter
}

// GetBlockFromNetwork gets a block by requesting it from the network, one peer
// at a time, until one answers.
func (s *ChainService) GetBlockFromNetwork(
	blockHash chainhash.Hash) *btcutil.Block {
	blockHeader, height, err := s.GetBlockByHash(blockHash)
	if err != nil || blockHeader.BlockHash() != blockHash {
		return nil
	}
	replyChan := make(chan *btcutil.Block)
	s.query <- getBlockMsg{
		blockHeader: &blockHeader,
		height:      height,
		reply:       replyChan,
	}
	block := <-replyChan
	if block != nil {
		log.Tracef("Got block %s from network", blockHash)
	}
	return block
}
