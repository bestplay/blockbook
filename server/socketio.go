package server

import (
	"blockbook/bchain"
	"blockbook/common"
	"blockbook/db"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/juju/errors"

	"github.com/golang/glog"
	"github.com/martinboehm/golang-socketio"
	"github.com/martinboehm/golang-socketio/transport"
)

const blockbookAbout = "Blockbook - blockchain indexer for TREZOR wallet https://trezor.io/. Do not use for any other purpose."

// SocketIoServer is handle to SocketIoServer
type SocketIoServer struct {
	binding     string
	certFiles   string
	server      *gosocketio.Server
	https       *http.Server
	db          *db.RocksDB
	txCache     *db.TxCache
	chain       bchain.BlockChain
	chainParser bchain.BlockChainParser
	explorerURL string
	metrics     *common.Metrics
	is          *common.InternalState
}

// NewSocketIoServer creates new SocketIo interface to blockbook and returns its handle
func NewSocketIoServer(binding string, certFiles string, db *db.RocksDB, chain bchain.BlockChain, txCache *db.TxCache, explorerURL string, metrics *common.Metrics, is *common.InternalState) (*SocketIoServer, error) {
	server := gosocketio.NewServer(transport.GetDefaultWebsocketTransport())

	server.On(gosocketio.OnConnection, func(c *gosocketio.Channel) {
		glog.Info("Client connected ", c.Id())
		metrics.SocketIOClients.Inc()
	})

	server.On(gosocketio.OnDisconnection, func(c *gosocketio.Channel) {
		glog.Info("Client disconnected ", c.Id())
		metrics.SocketIOClients.Dec()
	})

	server.On(gosocketio.OnError, func(c *gosocketio.Channel) {
		glog.Error("Client error ", c.Id())
	})

	type Message struct {
		Name    string `json:"name"`
		Message string `json:"message"`
	}

	addr, path := splitBinding(binding)
	serveMux := http.NewServeMux()
	https := &http.Server{
		Addr:    addr,
		Handler: serveMux,
	}

	s := &SocketIoServer{
		binding:     binding,
		certFiles:   certFiles,
		https:       https,
		server:      server,
		db:          db,
		txCache:     txCache,
		chain:       chain,
		chainParser: chain.GetChainParser(),
		explorerURL: explorerURL,
		metrics:     metrics,
		is:          is,
	}

	// support for tests of socket.io interface
	serveMux.Handle(path+"test.html", http.FileServer(http.Dir("./static/")))
	// redirect to Bitcore for details of transaction
	serveMux.HandleFunc(path+"tx/", s.txRedirect)
	serveMux.HandleFunc(path+"address/", s.addressRedirect)
	// API call used to detect state of Blockbook
	serveMux.HandleFunc(path+"api/block-index/", s.apiBlockIndex)
	// handle socket.io
	serveMux.Handle(path+"socket.io/", server)
	// default handler
	serveMux.HandleFunc(path, s.index)

	server.On("message", s.onMessage)
	server.On("subscribe", s.onSubscribe)

	return s, nil
}

func splitBinding(binding string) (addr string, path string) {
	i := strings.Index(binding, "/")
	if i >= 0 {
		return binding[0:i], binding[i:]
	}
	return binding, "/"
}

// Run starts the server
func (s *SocketIoServer) Run() error {
	if s.certFiles == "" {
		glog.Info("socketio server starting to listen on ws://", s.https.Addr)
		return s.https.ListenAndServe()
	}
	glog.Info("socketio server starting to listen on wss://", s.https.Addr)
	return s.https.ListenAndServeTLS(fmt.Sprint(s.certFiles, ".crt"), fmt.Sprint(s.certFiles, ".key"))
}

// Close closes the server
func (s *SocketIoServer) Close() error {
	glog.Infof("socketio server closing")
	return s.https.Close()
}

// Shutdown shuts down the server
func (s *SocketIoServer) Shutdown(ctx context.Context) error {
	glog.Infof("socketio server shutdown")
	return s.https.Shutdown(ctx)
}

func joinURL(base string, part string) string {
	if len(base) > 0 {
		if len(base) > 0 && base[len(base)-1] == '/' && len(part) > 0 && part[0] == '/' {
			return base + part[1:]
		} else {
			return base + part
		}
	}
	return part
}

func (s *SocketIoServer) txRedirect(w http.ResponseWriter, r *http.Request) {
	if s.explorerURL != "" {
		http.Redirect(w, r, joinURL(s.explorerURL, r.URL.Path), 302)
		s.metrics.ExplorerViews.With(common.Labels{"action": "tx"}).Inc()
	}
}

func (s *SocketIoServer) addressRedirect(w http.ResponseWriter, r *http.Request) {
	if s.explorerURL != "" {
		http.Redirect(w, r, joinURL(s.explorerURL, r.URL.Path), 302)
		s.metrics.ExplorerViews.With(common.Labels{"action": "address"}).Inc()
	}
}

type resAboutBlockbookPublic struct {
	Coin            string    `json:"coin"`
	Host            string    `json:"host"`
	Version         string    `json:"version"`
	GitCommit       string    `json:"gitcommit"`
	BuildTime       string    `json:"buildtime"`
	InSync          bool      `json:"inSync"`
	BestHeight      uint32    `json:"bestHeight"`
	LastBlockTime   time.Time `json:"lastBlockTime"`
	InSyncMempool   bool      `json:"inSyncMempool"`
	LastMempoolTime time.Time `json:"lastMempoolTime"`
	About           string    `json:"about"`
}

func (s *SocketIoServer) index(w http.ResponseWriter, r *http.Request) {
	vi := common.GetVersionInfo()
	ss, bh, st := s.is.GetSyncState()
	ms, mt, _ := s.is.GetMempoolSyncState()
	a := resAboutBlockbookPublic{
		Coin:            s.is.Coin,
		Host:            s.is.Host,
		Version:         vi.Version,
		GitCommit:       vi.GitCommit,
		BuildTime:       vi.BuildTime,
		InSync:          ss,
		BestHeight:      bh,
		LastBlockTime:   st,
		InSyncMempool:   ms,
		LastMempoolTime: mt,
		About:           blockbookAbout,
	}
	buf, err := json.MarshalIndent(a, "", "    ")
	if err != nil {
		glog.Error(err)
	}
	w.Write(buf)
}

func (s *SocketIoServer) apiBlockIndex(w http.ResponseWriter, r *http.Request) {
	type resBlockIndex struct {
		BlockHash string `json:"blockHash"`
		About     string `json:"about"`
	}
	var err error
	var hash string
	height := -1
	if i := strings.LastIndexByte(r.URL.Path, '/'); i > 0 {
		if h, err := strconv.Atoi(r.URL.Path[i+1:]); err == nil {
			height = h
		}
	}
	if height >= 0 {
		hash, err = s.db.GetBlockHash(uint32(height))
	} else {
		_, hash, err = s.db.GetBestBlock()
	}
	if err != nil {
		glog.Error(err)
	} else {
		r := resBlockIndex{
			BlockHash: hash,
			About:     blockbookAbout,
		}
		json.NewEncoder(w).Encode(r)
	}
}

type addrOpts struct {
	Start            int  `json:"start"`
	End              int  `json:"end"`
	QueryMempoolOnly bool `json:"queryMempoolOnly"`
	From             int  `json:"from"`
	To               int  `json:"to"`
}

var onMessageHandlers = map[string]func(*SocketIoServer, json.RawMessage) (interface{}, error){
	"getAddressTxids": func(s *SocketIoServer, params json.RawMessage) (rv interface{}, err error) {
		addr, opts, err := unmarshalGetAddressRequest(params)
		if err == nil {
			rv, err = s.getAddressTxids(addr, &opts)
		}
		return
	},
	"getAddressHistory": func(s *SocketIoServer, params json.RawMessage) (rv interface{}, err error) {
		addr, opts, err := unmarshalGetAddressRequest(params)
		if err == nil {
			rv, err = s.getAddressHistory(addr, &opts)
		}
		return
	},
	"getBlockHeader": func(s *SocketIoServer, params json.RawMessage) (rv interface{}, err error) {
		height, hash, err := unmarshalGetBlockHeader(params)
		if err == nil {
			rv, err = s.getBlockHeader(height, hash)
		}
		return
	},
	"estimateSmartFee": func(s *SocketIoServer, params json.RawMessage) (rv interface{}, err error) {
		blocks, conservative, err := unmarshalEstimateSmartFee(params)
		if err == nil {
			rv, err = s.estimateSmartFee(blocks, conservative)
		}
		return
	},
	"estimateFee": func(s *SocketIoServer, params json.RawMessage) (rv interface{}, err error) {
		blocks, err := unmarshalEstimateFee(params)
		if err == nil {
			rv, err = s.estimateFee(blocks)
		}
		return
	},
	"getInfo": func(s *SocketIoServer, params json.RawMessage) (rv interface{}, err error) {
		return s.getInfo()
	},
	"getDetailedTransaction": func(s *SocketIoServer, params json.RawMessage) (rv interface{}, err error) {
		txid, err := unmarshalGetDetailedTransaction(params)
		if err == nil {
			rv, err = s.getDetailedTransaction(txid)
		}
		return
	},
	"sendTransaction": func(s *SocketIoServer, params json.RawMessage) (rv interface{}, err error) {
		tx, err := unmarshalStringParameter(params)
		if err == nil {
			rv, err = s.sendTransaction(tx)
		}
		return
	},
	"getMempoolEntry": func(s *SocketIoServer, params json.RawMessage) (rv interface{}, err error) {
		txid, err := unmarshalStringParameter(params)
		if err == nil {
			rv, err = s.getMempoolEntry(txid)
		}
		return
	},
}

type resultError struct {
	Error struct {
		Message string `json:"message"`
	} `json:"error"`
}

func (s *SocketIoServer) onMessage(c *gosocketio.Channel, req map[string]json.RawMessage) (rv interface{}) {
	var err error
	method := strings.Trim(string(req["method"]), "\"")
	defer func() {
		if r := recover(); r != nil {
			glog.Error(c.Id(), " onMessage ", method, " recovered from panic: ", r)
			e := resultError{}
			e.Error.Message = "Internal error"
			rv = e
		}
	}()
	t := time.Now()
	params := req["params"]
	defer s.metrics.SocketIOReqDuration.With(common.Labels{"method": method}).Observe(float64(time.Since(t)) / 1e3) // in microseconds
	f, ok := onMessageHandlers[method]
	if ok {
		rv, err = f(s, params)
	} else {
		err = errors.New("unknown method")
	}
	if err == nil {
		glog.V(1).Info(c.Id(), " onMessage ", method, " success")
		s.metrics.SocketIORequests.With(common.Labels{"method": method, "status": "success"}).Inc()
		return rv
	}
	glog.Error(c.Id(), " onMessage ", method, ": ", errors.ErrorStack(err))
	s.metrics.SocketIORequests.With(common.Labels{"method": method, "status": err.Error()}).Inc()
	e := resultError{}
	e.Error.Message = err.Error()
	return e
}

func unmarshalGetAddressRequest(params []byte) (addr []string, opts addrOpts, err error) {
	var p []json.RawMessage
	err = json.Unmarshal(params, &p)
	if err != nil {
		return
	}
	if len(p) != 2 {
		err = errors.New("incorrect number of parameters")
		return
	}
	err = json.Unmarshal(p[0], &addr)
	if err != nil {
		return
	}
	err = json.Unmarshal(p[1], &opts)
	return
}

// bitcore returns txids from the newest to the oldest, we have to revert the order
func uniqueTxidsInReverse(txids []string) []string {
	i := len(txids)
	ut := make([]string, i)
	txidsMap := make(map[string]struct{})
	for _, txid := range txids {
		_, e := txidsMap[txid]
		if !e {
			i--
			ut[i] = txid
			txidsMap[txid] = struct{}{}
		}
	}
	return ut[i:]
}

type resultAddressTxids struct {
	Result []string `json:"result"`
}

func (s *SocketIoServer) getAddressTxids(addr []string, opts *addrOpts) (res resultAddressTxids, err error) {
	txids := make([]string, 0)
	lower, higher := uint32(opts.End), uint32(opts.Start)
	for _, address := range addr {
		if !opts.QueryMempoolOnly {
			err = s.db.GetTransactions(address, lower, higher, func(txid string, vout uint32, isOutput bool) error {
				txids = append(txids, txid)
				return nil
			})
			if err != nil {
				return res, err
			}
		} else {
			m, err := s.chain.GetMempoolTransactions(address)
			if err != nil {
				return res, err
			}
			txids = append(txids, m...)
		}
	}
	res.Result = uniqueTxidsInReverse(txids)
	return res, nil
}

type addressHistoryIndexes struct {
	InputIndexes  []int `json:"inputIndexes"`
	OutputIndexes []int `json:"outputIndexes"`
}

type txInputs struct {
	OutputIndex int     `json:"outputIndex"`
	Script      *string `json:"script"`
	// ScriptAsm   *string `json:"scriptAsm"`
	Sequence int64   `json:"sequence"`
	Address  *string `json:"address"`
	Satoshis int64   `json:"satoshis"`
}

type txOutputs struct {
	Satoshis int64   `json:"satoshis"`
	Script   *string `json:"script"`
	// ScriptAsm   *string `json:"scriptAsm"`
	SpentTxID   *string `json:"spentTxId,omitempty"`
	SpentIndex  int     `json:"spentIndex,omitempty"`
	SpentHeight int     `json:"spentHeight,omitempty"`
	Address     *string `json:"address"`
}

type resTx struct {
	Hex string `json:"hex"`
	// BlockHash      string      `json:"blockHash,omitempty"`
	Height         int   `json:"height"`
	BlockTimestamp int64 `json:"blockTimestamp,omitempty"`
	// Version        int         `json:"version"`
	Hash     string `json:"hash"`
	Locktime int    `json:"locktime,omitempty"`
	// Size           int         `json:"size,omitempty"`
	Inputs []txInputs `json:"inputs"`
	// InputSatoshis  int64       `json:"inputSatoshis,omitempty"`
	Outputs []txOutputs `json:"outputs"`
	// OutputSatoshis int64       `json:"outputSatoshis,omitempty"`
	// FeeSatoshis    int64       `json:"feeSatoshis,omitempty"`
}

type addressHistoryItem struct {
	Addresses     map[string]addressHistoryIndexes `json:"addresses"`
	Satoshis      int64                            `json:"satoshis"`
	Confirmations int                              `json:"confirmations"`
	Tx            resTx                            `json:"tx"`
}

type resultGetAddressHistory struct {
	Result struct {
		TotalCount int                  `json:"totalCount"`
		Items      []addressHistoryItem `json:"items"`
	} `json:"result"`
}

func stringInSlice(a string, list []string) bool {
	for _, b := range list {
		if b == a {
			return true
		}
	}
	return false
}

func txToResTx(tx *bchain.Tx, height int, hi []txInputs, ho []txOutputs) resTx {
	return resTx{
		// BlockHash:      tx.BlockHash,
		BlockTimestamp: tx.Blocktime,
		// FeeSatoshis,
		Hash:   tx.Txid,
		Height: height,
		Hex:    tx.Hex,
		Inputs: hi,
		// InputSatoshis,
		Locktime: int(tx.LockTime),
		Outputs:  ho,
		// OutputSatoshis,
		// Version: int(tx.Version),
	}
}

func (s *SocketIoServer) getAddressHistory(addr []string, opts *addrOpts) (res resultGetAddressHistory, err error) {
	txr, err := s.getAddressTxids(addr, opts)
	if err != nil {
		return
	}
	bestheight, _, err := s.db.GetBestBlock()
	if err != nil {
		return
	}
	txids := txr.Result
	res.Result.TotalCount = len(txids)
	res.Result.Items = make([]addressHistoryItem, 0)
	for i, txid := range txids {
		if i >= opts.From && i < opts.To {
			tx, height, err := s.txCache.GetTransaction(txid, bestheight)
			if err != nil {
				return res, err
			}
			ads := make(map[string]addressHistoryIndexes)
			hi := make([]txInputs, 0)
			ho := make([]txOutputs, 0)
			for _, vout := range tx.Vout {
				aoh := vout.ScriptPubKey.Hex
				ao := txOutputs{
					Satoshis: int64(vout.Value*1E8 + 0.5),
					Script:   &aoh,
				}
				if vout.Address != nil {
					a := vout.Address.String()
					ao.Address = &a
					if vout.Address.InSlice(addr) {
						hi, ok := ads[a]
						if ok {
							hi.OutputIndexes = append(hi.OutputIndexes, int(vout.N))
						} else {
							hi := addressHistoryIndexes{}
							hi.InputIndexes = make([]int, 0)
							hi.OutputIndexes = append(hi.OutputIndexes, int(vout.N))
							ads[a] = hi
						}
					}
				}
				ho = append(ho, ao)
			}
			ahi := addressHistoryItem{}
			ahi.Addresses = ads
			ahi.Confirmations = int(tx.Confirmations)
			var h int
			if tx.Confirmations == 0 {
				h = -1
			} else {
				h = int(height)
			}
			ahi.Tx = txToResTx(tx, h, hi, ho)
			res.Result.Items = append(res.Result.Items, ahi)
		}
	}
	return
}

func unmarshalArray(params []byte, np int) (p []interface{}, err error) {
	err = json.Unmarshal(params, &p)
	if err != nil {
		return
	}
	if len(p) != np {
		err = errors.New("incorrect number of parameters")
		return
	}
	return
}

func unmarshalGetBlockHeader(params []byte) (height uint32, hash string, err error) {
	p, err := unmarshalArray(params, 1)
	if err != nil {
		return
	}
	fheight, ok := p[0].(float64)
	if ok {
		return uint32(fheight), "", nil
	}
	hash, ok = p[0].(string)
	if ok {
		return
	}
	err = errors.New("incorrect parameter")
	return
}

type resultGetBlockHeader struct {
	Result struct {
		Hash          string  `json:"hash"`
		Version       int     `json:"version"`
		Confirmations int     `json:"confirmations"`
		Height        int     `json:"height"`
		ChainWork     string  `json:"chainWork"`
		NextHash      string  `json:"nextHash"`
		MerkleRoot    string  `json:"merkleRoot"`
		Time          int     `json:"time"`
		MedianTime    int     `json:"medianTime"`
		Nonce         int     `json:"nonce"`
		Bits          string  `json:"bits"`
		Difficulty    float64 `json:"difficulty"`
	} `json:"result"`
}

func (s *SocketIoServer) getBlockHeader(height uint32, hash string) (res resultGetBlockHeader, err error) {
	if hash == "" {
		// trezor is interested only in hash
		hash, err = s.db.GetBlockHash(height)
		if err != nil {
			return
		}
		res.Result.Hash = hash
		return
	}
	bh, err := s.chain.GetBlockHeader(hash)
	if err != nil {
		return
	}
	res.Result.Hash = bh.Hash
	res.Result.Confirmations = bh.Confirmations
	res.Result.Height = int(bh.Height)
	res.Result.NextHash = bh.Next
	return
}

func unmarshalEstimateSmartFee(params []byte) (blocks int, conservative bool, err error) {
	p, err := unmarshalArray(params, 2)
	if err != nil {
		return
	}
	fblocks, ok := p[0].(float64)
	if !ok {
		err = errors.New("Invalid parameter blocks")
		return
	}
	blocks = int(fblocks)
	conservative, ok = p[1].(bool)
	if !ok {
		err = errors.New("Invalid parameter conservative")
		return
	}
	return
}

type resultEstimateSmartFee struct {
	Result float64 `json:"result"`
}

func (s *SocketIoServer) estimateSmartFee(blocks int, conservative bool) (res resultEstimateSmartFee, err error) {
	fee, err := s.chain.EstimateSmartFee(blocks, conservative)
	if err != nil {
		return
	}
	res.Result = fee
	return
}

func unmarshalEstimateFee(params []byte) (blocks int, err error) {
	p, err := unmarshalArray(params, 1)
	if err != nil {
		return
	}
	fblocks, ok := p[0].(float64)
	if !ok {
		err = errors.New("Invalid parameter nblocks")
		return
	}
	blocks = int(fblocks)
	return
}

type resultEstimateFee struct {
	Result float64 `json:"result"`
}

func (s *SocketIoServer) estimateFee(blocks int) (res resultEstimateFee, err error) {
	fee, err := s.chain.EstimateFee(blocks)
	if err != nil {
		return
	}
	res.Result = fee
	return
}

type resultGetInfo struct {
	Result struct {
		Version         int     `json:"version,omitempty"`
		ProtocolVersion int     `json:"protocolVersion,omitempty"`
		Blocks          int     `json:"blocks"`
		TimeOffset      int     `json:"timeOffset,omitempty"`
		Connections     int     `json:"connections,omitempty"`
		Proxy           string  `json:"proxy,omitempty"`
		Difficulty      float64 `json:"difficulty,omitempty"`
		Testnet         bool    `json:"testnet"`
		RelayFee        float64 `json:"relayFee,omitempty"`
		Errors          string  `json:"errors,omitempty"`
		Network         string  `json:"network,omitempty"`
		Subversion      string  `json:"subversion,omitempty"`
		LocalServices   string  `json:"localServices,omitempty"`
		CoinName        string  `json:"coin_name,omitempty"`
		About           string  `json:"about,omitempty"`
	} `json:"result"`
}

func (s *SocketIoServer) getInfo() (res resultGetInfo, err error) {
	height, _, err := s.db.GetBestBlock()
	if err != nil {
		return
	}
	res.Result.Blocks = int(height)
	res.Result.Testnet = s.chain.IsTestnet()
	res.Result.Network = s.chain.GetNetworkName()
	res.Result.Subversion = s.chain.GetSubversion()
	res.Result.CoinName = s.chain.GetCoinName()
	res.Result.About = blockbookAbout
	return
}

func unmarshalStringParameter(params []byte) (s string, err error) {
	p, err := unmarshalArray(params, 1)
	if err != nil {
		return
	}
	s, ok := p[0].(string)
	if ok {
		return
	}
	err = errors.New("incorrect parameter")
	return
}

func unmarshalGetDetailedTransaction(params []byte) (txid string, err error) {
	var p []json.RawMessage
	err = json.Unmarshal(params, &p)
	if err != nil {
		return
	}
	if len(p) != 1 {
		err = errors.New("incorrect number of parameters")
		return
	}
	err = json.Unmarshal(p[0], &txid)
	if err != nil {
		return
	}
	return
}

type resultGetDetailedTransaction struct {
	Result resTx `json:"result"`
}

func (s *SocketIoServer) getDetailedTransaction(txid string) (res resultGetDetailedTransaction, err error) {
	bestheight, _, err := s.db.GetBestBlock()
	if err != nil {
		return
	}
	tx, height, err := s.txCache.GetTransaction(txid, bestheight)
	if err != nil {
		return res, err
	}
	hi := make([]txInputs, 0)
	ho := make([]txOutputs, 0)
	for _, vin := range tx.Vin {
		ais := vin.ScriptSig.Hex
		ai := txInputs{
			Script:      &ais,
			Sequence:    int64(vin.Sequence),
			OutputIndex: int(vin.Vout),
		}
		if vin.Txid != "" {
			otx, _, err := s.txCache.GetTransaction(vin.Txid, bestheight)
			if err != nil {
				return res, err
			}
			if len(otx.Vout) > int(vin.Vout) {
				vout := otx.Vout[vin.Vout]
				if vout.Address != nil {
					a := vout.Address.String()
					ai.Address = &a
				}
				ai.Satoshis = int64(vout.Value*1E8 + 0.5)
			}
		}
		hi = append(hi, ai)
	}
	for _, vout := range tx.Vout {
		aos := vout.ScriptPubKey.Hex
		ao := txOutputs{
			Satoshis: int64(vout.Value*1E8 + 0.5),
			Script:   &aos,
		}
		if vout.Address != nil {
			a := vout.Address.String()
			ao.Address = &a
		}
		ho = append(ho, ao)
	}
	var h int
	if tx.Confirmations == 0 {
		h = -1
	} else {
		h = int(height)
	}
	res.Result = txToResTx(tx, h, hi, ho)
	return
}

type resultSendTransaction struct {
	Result string `json:"result"`
}

func (s *SocketIoServer) sendTransaction(tx string) (res resultSendTransaction, err error) {
	txid, err := s.chain.SendRawTransaction(tx)
	if err != nil {
		return res, err
	}
	res.Result = txid
	return
}

type resultGetMempoolEntry struct {
	Result *bchain.MempoolEntry `json:"result"`
}

func (s *SocketIoServer) getMempoolEntry(txid string) (res resultGetMempoolEntry, err error) {
	entry, err := s.chain.GetMempoolEntry(txid)
	if err != nil {
		return res, err
	}
	res.Result = entry
	return
}

// onSubscribe expects two event subscriptions based on the req parameter (including the doublequotes):
// "bitcoind/hashblock"
// "bitcoind/addresstxid",["2MzTmvPJLZaLzD9XdN3jMtQA5NexC3rAPww","2NAZRJKr63tSdcTxTN3WaE9ZNDyXy6PgGuv"]
func (s *SocketIoServer) onSubscribe(c *gosocketio.Channel, req []byte) interface{} {
	defer func() {
		if r := recover(); r != nil {
			glog.Error(c.Id(), " onSubscribe recovered from panic: ", r)
		}
	}()

	onError := func(id, sc, err, detail string) {
		glog.Error(id, " onSubscribe ", err, ": ", detail)
		s.metrics.SocketIOSubscribes.With(common.Labels{"channel": sc, "status": err}).Inc()
	}

	r := string(req)
	glog.V(1).Info(c.Id(), " onSubscribe ", r)
	var sc string
	i := strings.Index(r, "\",[")
	if i > 0 {
		var addrs []string
		sc = r[1:i]
		if sc != "bitcoind/addresstxid" {
			onError(c.Id(), sc, "invalid data", "expecting bitcoind/addresstxid, req: "+r)
			return nil
		}
		err := json.Unmarshal([]byte(r[i+2:]), &addrs)
		if err != nil {
			onError(c.Id(), sc, "invalid data", err.Error()+", req: "+r)
			return nil
		}
		for _, a := range addrs {
			c.Join("bitcoind/addresstxid-" + a)
		}
	} else {
		sc = r[1 : len(r)-1]
		if sc != "bitcoind/hashblock" {
			onError(c.Id(), sc, "invalid data", "expecting bitcoind/hashblock, req: "+r)
			return nil
		}
		c.Join(sc)
	}
	s.metrics.SocketIOSubscribes.With(common.Labels{"channel": sc, "status": "success"}).Inc()
	return nil
}

// OnNewBlockHash notifies users subscribed to bitcoind/hashblock about new block
func (s *SocketIoServer) OnNewBlockHash(hash string) {
	c := s.server.BroadcastTo("bitcoind/hashblock", "bitcoind/hashblock", hash)
	glog.Info("broadcasting new block hash ", hash, " to ", c, " channels")
}

// OnNewTxAddr notifies users subscribed to bitcoind/addresstxid about new block
func (s *SocketIoServer) OnNewTxAddr(txid string, addr string) {
	c := s.server.BroadcastTo("bitcoind/addresstxid-"+addr, "bitcoind/addresstxid", map[string]string{"address": addr, "txid": txid})
	if c > 0 {
		glog.Info("broadcasting new txid ", txid, " for addr ", addr, " to ", c, " channels")
	}
}
