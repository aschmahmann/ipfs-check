package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"net/url"
	"sync"
	"time"

	"github.com/ipfs/go-cid"
	"github.com/ipfs/go-ipns"
	"github.com/libp2p/go-libp2p"
	"github.com/libp2p/go-libp2p-connmgr"
	"github.com/libp2p/go-libp2p-core/host"
	"github.com/libp2p/go-libp2p-core/peer"
	"github.com/libp2p/go-libp2p-core/routing"
	"github.com/libp2p/go-libp2p-kad-dht"
	"github.com/libp2p/go-libp2p-kad-dht/fullrt"
	dhtpb "github.com/libp2p/go-libp2p-kad-dht/pb"
	record "github.com/libp2p/go-libp2p-record"
	"github.com/multiformats/go-multiaddr"
)

type kademlia interface {
	routing.Routing
	GetClosestPeers(ctx context.Context, key string) ([]peer.ID, error)
}

func main() {
	h, err := libp2p.New(
		libp2p.ConnectionManager(connmgr.NewConnManager(600, 900, time.Second*30)),
		libp2p.ConnectionGater(&privateAddrFilterConnectionGater{}),
	)
	if err != nil {
		panic(err)
	}

	d, err := fullrt.NewFullRT(h, "/ipfs",
		fullrt.DHTOption(
			dht.BucketSize(20),
			dht.Validator(record.NamespacedValidator{
				"pk":   record.PublicKeyValidator{},
				"ipns": ipns.Validator{},
			}),
			dht.BootstrapPeers(dht.GetDefaultBootstrapPeerAddrInfos()...),
			dht.Mode(dht.ModeClient),
		))

	if err != nil {
		panic(err)
	}

	pm, err := dhtProtocolMessenger("/ipfs/kad/1.0.0", h)
	if err != nil {
		panic(err)
	}

	daemon := &daemon{h: h, dht: d, dhtMessenger: pm}

	l, err := net.Listen("tcp", ":3333")
	if err != nil {
		panic(err)
	}

	fmt.Printf("listening on %v\n", l.Addr())

	// Wait for the DHT to be ready
	for !d.Ready() {
		time.Sleep(time.Second * 10)
	}

	fmt.Println("Ready to start serving")

	/*
		1. Is the peer findable in the DHT?
		2. Does the multiaddr work? (what's the error)
		3. Is the CID in the DHT?
		4. Does the peer respond that it has the given data over Bitswap?
	*/

	http.HandleFunc("/", func(writer http.ResponseWriter, request *http.Request) {
		if err := daemon.runCheck(writer, request.RequestURI); err != nil {
			writer.Header().Add("Access-Control-Allow-Origin", "*")
			writer.WriteHeader(http.StatusInternalServerError)
			_, _ = writer.Write([]byte(err.Error()))
			return
		}
	})

	err = http.Serve(l, nil)
	if err != nil {
		panic(err)
	}
}

type daemon struct {
	h            host.Host
	dht          kademlia
	dhtMessenger *dhtpb.ProtocolMessenger
}

func (d *daemon) runCheck(writer http.ResponseWriter, uristr string) error {
	u, err := url.ParseRequestURI(uristr)
	if err != nil {
		return err
	}

	mastr := u.Query().Get("multiaddr")
	cidstr := u.Query().Get("cid")

	if mastr == "" || cidstr == "" {
		return errors.New("missing argument")
	}

	ai, err := peer.AddrInfoFromString(mastr)
	if err != nil {
		return err
	}

	c, err := cid.Decode(cidstr)
	if err != nil {
		return err
	}

	ctx := context.Background()
	out := &Output{}

	connectionFailed := false

	out.CidInDHT = providerRecordInDHT(ctx, d.dht, c, ai.ID)

	addrMap, peerAddrDHTErr := peerAddrsInDHT(ctx, d.dht, d.dhtMessenger, ai.ID)
	out.PeerFoundInDHT = addrMap

	// If peerID given, but no addresses check the DHT
	if len(ai.Addrs) == 0 {
		if peerAddrDHTErr != nil {
			connectionFailed = true
			out.ConnectionError = peerAddrDHTErr.Error()
		}
		for a := range addrMap {
			ma, err := multiaddr.NewMultiaddr(a)
			if err != nil {
				log.Println(fmt.Errorf("error parsing multiaddr %s: %w", a, err))
				continue
			}
			ai.Addrs = append(ai.Addrs, ma)
		}
	}

	testHost, err := libp2p.New(libp2p.ConnectionGater(&privateAddrFilterConnectionGater{}))
	if err != nil {
		return fmt.Errorf("server error: %w", err)
	}
	defer testHost.Close()

	if !connectionFailed {
		// Is the target connectable
		dialCtx, dialCancel := context.WithTimeout(ctx, time.Second*3)
		connErr := testHost.Connect(dialCtx, *ai)
		dialCancel()
		if connErr != nil {
			out.ConnectionError = connErr.Error()
			connectionFailed = true
		}
	}

	if connectionFailed {
		out.DataAvailableOverBitswap.Error = "could not connect to peer"
	} else {
		// If so is the data available over Bitswap
		bsOut := checkBitswapCID(ctx, testHost, c, *ai)
		out.DataAvailableOverBitswap = *bsOut
	}

	outputData, err := json.Marshal(out)
	if err != nil {
		return err
	}

	writer.Header().Add("Access-Control-Allow-Origin", "*")
	_, err = writer.Write(outputData)
	if err != nil {
		fmt.Printf("could not return data over HTTP: %v\n", err.Error())
	}

	return nil
}

type Output struct {
	ConnectionError          string
	PeerFoundInDHT           map[string]int
	CidInDHT                 bool
	DataAvailableOverBitswap BsCheckOutput
}

func peerAddrsInDHT(ctx context.Context, d kademlia, messenger *dhtpb.ProtocolMessenger, p peer.ID) (map[string]int, error) {
	closestPeers, err := d.GetClosestPeers(ctx, string(p))
	if err != nil {
		return nil, err
	}

	wg := sync.WaitGroup{}
	wg.Add(len(closestPeers))

	resCh := make(chan *peer.AddrInfo, len(closestPeers))

	numSuccessfulResponses := execOnMany(ctx, 0.3, time.Second*3, func(ctx context.Context, peerToQuery peer.ID) error {
		endResults, err := messenger.GetClosestPeers(ctx, peerToQuery, p)
		if err == nil {
			for _, r := range endResults {
				if r.ID == p {
					resCh <- r
					return nil
				}
			}
			resCh <- nil
		}
		return err
	}, closestPeers, false)
	close(resCh)

	if numSuccessfulResponses == 0 {
		return nil, fmt.Errorf("host had trouble querying the DHT")
	}

	addrMap := make(map[string]int)
	for r := range resCh {
		if r == nil {
			continue
		}
		for _, addr := range r.Addrs {
			addrMap[addr.String()]++
		}
	}

	return addrMap, nil
}

func providerRecordInDHT(ctx context.Context, d kademlia, c cid.Cid, p peer.ID) bool {
	queryCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	provsCh := d.FindProvidersAsync(queryCtx, c, 0)
	for {
		select {
		case prov, ok := <-provsCh:
			if !ok {
				return false
			}
			if prov.ID == p {
				return true
			}
		case <-ctx.Done():
			return false
		}
	}
}

// Taken from the FullRT DHT client implementation
//
// execOnMany executes the given function on each of the peers, although it may only wait for a certain chunk of peers
// to respond before considering the results "good enough" and returning.
//
// If sloppyExit is true then this function will return without waiting for all of its internal goroutines to close.
// If sloppyExit is true then the passed in function MUST be able to safely complete an arbitrary amount of time after
// execOnMany has returned (e.g. do not write to resources that might get closed or set to nil and therefore result in
// a panic instead of just returning an error).
func execOnMany(ctx context.Context, waitFrac float64, timeoutPerOp time.Duration, fn func(context.Context, peer.ID) error, peers []peer.ID, sloppyExit bool) int {
	if len(peers) == 0 {
		return 0
	}

	// having a buffer that can take all of the elements is basically a hack to allow for sloppy exits that clean up
	// the goroutines after the function is done rather than before
	errCh := make(chan error, len(peers))
	numSuccessfulToWaitFor := int(float64(len(peers)) * waitFrac)

	putctx, cancel := context.WithTimeout(ctx, timeoutPerOp)
	defer cancel()

	for _, p := range peers {
		go func(p peer.ID) {
			errCh <- fn(putctx, p)
		}(p)
	}

	var numDone, numSuccess, successSinceLastTick int
	var ticker *time.Ticker
	var tickChan <-chan time.Time

	for numDone < len(peers) {
		select {
		case err := <-errCh:
			numDone++
			if err == nil {
				numSuccess++
				if numSuccess >= numSuccessfulToWaitFor && ticker == nil {
					// Once there are enough successes, wait a little longer
					ticker = time.NewTicker(time.Millisecond * 500)
					defer ticker.Stop()
					tickChan = ticker.C
					successSinceLastTick = numSuccess
				}
				// This is equivalent to numSuccess * 2 + numFailures >= len(peers) and is a heuristic that seems to be
				// performing reasonably.
				// TODO: Make this metric more configurable
				// TODO: Have better heuristics in this function whether determined from observing static network
				// properties or dynamically calculating them
				if numSuccess+numDone >= len(peers) {
					cancel()
					if sloppyExit {
						return numSuccess
					}
				}
			}
		case <-tickChan:
			if numSuccess > successSinceLastTick {
				// If there were additional successes, then wait another tick
				successSinceLastTick = numSuccess
			} else {
				cancel()
				if sloppyExit {
					return numSuccess
				}
			}
		}
	}
	return numSuccess
}
