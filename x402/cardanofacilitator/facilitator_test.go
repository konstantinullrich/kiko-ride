package cardanofacilitator

import (
	"context"
	"encoding/base64"
	"encoding/hex"
	"math/big"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/blinklabs-io/gouroboros/cbor"
	"github.com/blinklabs-io/gouroboros/ledger/babbage"
	"github.com/blinklabs-io/gouroboros/ledger/common"
	"github.com/blinklabs-io/gouroboros/ledger/conway"
	"github.com/blinklabs-io/gouroboros/ledger/mary"
	"github.com/blinklabs-io/gouroboros/ledger/shelley"

	"kiko-backend/x402"
)

const (
	payTo  = "addr_test1wzgsasrmtg2mafhrjfwkakuccvvcmqckcsl7dp7wlmug0mq5ajkke"
	payer  = "addr_test1vz2fxv2umyhttkxyxp8x0dlpdt3k6cwng5pxj3jhsydzerspjrlsz"
	fare   = 3_000_000
	fee    = 200_000
	inSlot = 1_000
)

var (
	inputID = common.Blake2b256{7}
	nonce   = hex.EncodeToString(inputID.Bytes()) + "#0"
)

// fakeChain is a chain at slot 1,000 where the payer holds 10 ADA.
type fakeChain struct {
	mu        sync.Mutex
	slot      uint64
	outputs   map[string]Output
	submitted [][]byte
	reject    error
	// confirmations is what Confirmations answers per call; the last one
	// repeats. Empty means the transaction is in no block.
	confirmations []int
}

func newChain() *fakeChain {
	return &fakeChain{slot: inSlot, outputs: map[string]Output{
		nonce: {Address: payer, Lovelace: 10_000_000},
	}}
}

func (c *fakeChain) Tip(context.Context) (Tip, error) {
	return Tip{Slot: c.slot, Time: time.Now()}, nil
}

func (c *fakeChain) Params(context.Context) (Params, error) {
	return Params{MinFeeA: 44, MinFeeB: 155_381, CoinsPerUTxOByte: 4_310, SlotLength: time.Second}, nil
}

func (c *fakeChain) Outputs(_ context.Context, refs []string) (map[string]Output, error) {
	out := map[string]Output{}
	for _, r := range refs {
		if o, ok := c.outputs[r]; ok {
			out[r] = o
		}
	}
	return out, nil
}

func (c *fakeChain) Submit(_ context.Context, tx []byte) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.reject != nil {
		return c.reject
	}
	c.submitted = append(c.submitted, tx)
	return nil
}

func (c *fakeChain) Confirmations(context.Context, string) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.confirmations) == 0 {
		return -1, nil
	}
	n := c.confirmations[0]
	if len(c.confirmations) > 1 {
		c.confirmations = c.confirmations[1:]
	}
	return n, nil
}

type txOptions struct {
	amount, fee, change uint64
	ttl                 uint64
	to                  string
	mutate              func(*conway.ConwayTransaction)
}

// newPayment builds a payment of the payer's 10 ADA input, by default fare to
// payTo with the rest as change, valid until slot 1,300.
func newPayment(t *testing.T, o txOptions) (x402.PaymentPayload, x402.PaymentRequirements) {
	t.Helper()
	if o.amount == 0 {
		o.amount = fare
	}
	if o.fee == 0 {
		o.fee = fee
	}
	if o.change == 0 {
		o.change = 10_000_000 - o.amount - o.fee
	}
	if o.ttl == 0 {
		o.ttl = inSlot + 300
	}
	if o.to == "" {
		o.to = payTo
	}
	addr := func(s string) common.Address {
		a, err := common.NewAddress(s)
		if err != nil {
			t.Fatal(err)
		}
		return a
	}
	tx := conway.ConwayTransaction{
		Body: conway.ConwayTransactionBody{
			TxInputs: conway.NewConwayTransactionInputSet([]shelley.ShelleyTransactionInput{{TxId: inputID, OutputIndex: 0}}),
			TxOutputs: []babbage.BabbageTransactionOutput{
				{OutputAddress: addr(o.to), OutputAmount: mary.MaryTransactionOutputValue{Amount: o.amount}},
				{OutputAddress: addr(payer), OutputAmount: mary.MaryTransactionOutputValue{Amount: o.change}},
			},
			TxFee: o.fee,
			Ttl:   o.ttl,
		},
		TxIsValid: true,
	}
	if o.mutate != nil {
		o.mutate(&tx)
	}
	raw, err := cbor.Encode(&tx)
	if err != nil {
		t.Fatal(err)
	}
	req := x402.PaymentRequirements{
		Scheme: "exact", Network: x402.Preprod, Amount: "3000000", Asset: x402.Lovelace,
		PayTo: payTo, MaxTimeoutSeconds: 600,
	}
	return x402.PaymentPayload{
		X402Version: x402.Version,
		Accepted:    req,
		Payload: x402.TransactionPayload{
			Transaction: base64.StdEncoding.EncodeToString(raw),
			Nonce:       nonce,
		},
	}, req
}

func newFacilitator(c *fakeChain) *Facilitator {
	return &Facilitator{Network: x402.Preprod, Chain: c, Wait: 50 * time.Millisecond, Poll: time.Millisecond}
}

func TestVerify(t *testing.T) {
	c := newChain()
	f := newFacilitator(c)
	p, req := newPayment(t, txOptions{})
	v, err := f.Verify(context.Background(), p, req)
	if err != nil || !v.IsValid || v.Payer != payer {
		t.Fatalf("valid payment refused: %+v %v", v, err)
	}
	if len(c.submitted) != 0 {
		t.Fatal("verify submitted")
	}

	plutus := func(tx *conway.ConwayTransaction) {
		tx.Body.TxCollateral = cbor.NewSetType([]shelley.ShelleyTransactionInput{{TxId: inputID}}, false)
	}
	mint := func(tx *conway.ConwayTransaction) {
		ma := common.NewMultiAsset[common.MultiAssetTypeMint](map[common.Blake2b224]map[cbor.ByteString]common.MultiAssetTypeMint{
			{1}: {cbor.NewByteString([]byte("x")): big.NewInt(1)},
		})
		tx.Body.TxMint = &ma
	}
	badNonce := func(p *x402.PaymentPayload) { p.Payload.Nonce = strings.Repeat("ab", 32) + "#0" }
	for name, tc := range map[string]struct {
		opts   txOptions
		chain  func(*fakeChain)
		pay    func(*x402.PaymentPayload)
		reason string
	}{
		"underpaid":       {opts: txOptions{amount: fare - 1}, reason: ReasonInvalidPayment},
		"wrong recipient": {opts: txOptions{to: payer}, reason: ReasonInvalidPayment},
		"unbalanced":      {opts: txOptions{change: 7_000_000}, reason: ReasonInvalidTransaction},
		"fee too low":     {opts: txOptions{fee: 1_000}, reason: ReasonInvalidTransaction},
		"no ttl":          {opts: txOptions{mutate: func(tx *conway.ConwayTransaction) { tx.Body.Ttl = 0 }}, reason: ReasonInvalidTransaction},
		"ttl passed":      {opts: txOptions{ttl: inSlot}, reason: ReasonExpired},
		"ttl too far":     {opts: txOptions{ttl: inSlot + 601}, reason: ReasonInvalidTransaction},
		"below min utxo":  {opts: txOptions{amount: 500_000}, pay: func(p *x402.PaymentPayload) { p.Accepted.Amount = "500000" }, reason: ReasonInvalidPayment},
		"spent input":     {chain: func(c *fakeChain) { o := c.outputs[nonce]; o.Spent = true; c.outputs[nonce] = o }, reason: ReasonInvalidTransaction},
		"unknown input":   {chain: func(c *fakeChain) { c.outputs = map[string]Output{} }, reason: ReasonInvalidTransaction},
		"nonce not spent": {pay: badNonce, reason: ReasonInvalidNonce},
		"plutus":          {opts: txOptions{mutate: plutus}, reason: ReasonInvalidTransaction},
		"mints":           {opts: txOptions{mutate: mint}, reason: ReasonInvalidTransaction},
		"mainnet network": {opts: txOptions{mutate: func(tx *conway.ConwayTransaction) { one := uint8(1); tx.Body.TxNetworkId = &one }}, reason: ReasonInvalidNetwork},
		"not base64":      {pay: func(p *x402.PaymentPayload) { p.Payload.Transaction = "!" }, reason: x402.ReasonInvalidPayload},
	} {
		t.Run(name, func(t *testing.T) {
			c := newChain()
			if tc.chain != nil {
				tc.chain(c)
			}
			p, req := newPayment(t, tc.opts)
			if tc.pay != nil {
				tc.pay(&p)
				req.Amount = p.Accepted.Amount
			}
			v, err := newFacilitator(c).Verify(context.Background(), p, req)
			if err != nil {
				t.Fatal(err)
			}
			if tc.reason == "" {
				if !v.IsValid {
					t.Fatalf("refused: %s", v.InvalidReason)
				}
				return
			}
			if v.IsValid || !strings.HasPrefix(v.InvalidReason, tc.reason+":") {
				t.Fatalf("expected %s, got %+v", tc.reason, v)
			}
		})
	}
}

func TestSettleWaitsForConfirmation(t *testing.T) {
	c := newChain()
	f := newFacilitator(c)
	p, req := newPayment(t, txOptions{})
	c.confirmations = []int{-1, -1, 0, 1}
	s, err := f.Settle(context.Background(), p, req)
	if err != nil || !s.Success || s.Extra["confirmations"] != 1 || s.Extra["status"] != "confirmed" || s.Payer != payer {
		t.Fatalf("unexpected settlement: %+v %v", s, err)
	}
	if len(c.submitted) != 1 {
		t.Fatalf("submitted %d times", len(c.submitted))
	}
	if want, _ := base64.StdEncoding.DecodeString(p.Payload.Transaction); string(c.submitted[0]) != string(want) {
		t.Fatal("submitted bytes differ from the payload")
	}

	// Once on chain its input is spent, yet the same payment still verifies
	// and settles without being submitted again.
	o := c.outputs[p.Payload.Nonce]
	o.Spent = true
	c.outputs[p.Payload.Nonce] = o
	if v, err := newFacilitator(c).Verify(context.Background(), p, req); err != nil || !v.IsValid {
		t.Fatalf("landed payment refused: %+v %v", v, err)
	}
	if s, err := newFacilitator(c).Settle(context.Background(), p, req); err != nil || !s.Success || len(c.submitted) != 1 {
		t.Fatalf("landed payment not settled: %+v %v, %d submissions", s, err, len(c.submitted))
	}
}

func TestSettlePendingResumes(t *testing.T) {
	c := newChain()
	f := newFacilitator(c)
	p, req := newPayment(t, txOptions{})
	s, err := f.Settle(context.Background(), p, req)
	if err != nil || s.Success || s.ErrorReason != x402.ReasonSettlementPending || s.Transaction == "" {
		t.Fatalf("expected pending, got %+v %v", s, err)
	}
	// The retry watches the same transaction instead of sending it again.
	c.confirmations = []int{2}
	s, err = f.Settle(context.Background(), p, req)
	if err != nil || !s.Success || len(c.submitted) != 1 {
		t.Fatalf("retry: %+v %v, %d submissions", s, err, len(c.submitted))
	}
}

func TestSettleExpires(t *testing.T) {
	c := newChain()
	f := newFacilitator(c)
	p, req := newPayment(t, txOptions{})
	if s, _ := f.Settle(context.Background(), p, req); s.ErrorReason != x402.ReasonSettlementPending {
		t.Fatalf("expected pending, got %+v", s)
	}
	c.slot = inSlot + 300
	s, err := f.Settle(context.Background(), p, req)
	if err != nil || s.Success || s.ErrorReason != ReasonExpired || len(c.submitted) != 1 {
		t.Fatalf("expected expiry, got %+v %v, %d submissions", s, err, len(c.submitted))
	}
}

func TestSettleSubmission(t *testing.T) {
	p, req := newPayment(t, txOptions{})

	c := newChain()
	c.reject = &Rejected{Reason: "BadInputsUTxO"}
	s, err := newFacilitator(c).Settle(context.Background(), p, req)
	if err != nil || s.Success || !strings.HasPrefix(s.ErrorReason, ReasonSubmissionRejected+": BadInputsUTxO") {
		t.Fatalf("expected rejection, got %+v %v", s, err)
	}

	// Unknown whether it went out: pending, so the claim on it is kept.
	c = newChain()
	c.reject = context.DeadlineExceeded
	f := newFacilitator(c)
	if s, err := f.Settle(context.Background(), p, req); err != nil || s.ErrorReason != x402.ReasonSettlementPending {
		t.Fatalf("expected pending, got %+v %v", s, err)
	}
	c.reject, c.confirmations = nil, []int{1}
	if s, err := f.Settle(context.Background(), p, req); err != nil || !s.Success || len(c.submitted) != 0 {
		t.Fatalf("retry: %+v %v, %d submissions", s, err, len(c.submitted))
	}

	// A mempool policy settles on the node's acceptance.
	c = newChain()
	req.Extra = map[string]any{"confirmationPolicy": map[string]any{"l1Confirmations": float64(-1)}}
	p.Accepted.Extra = req.Extra
	if s, err := newFacilitator(c).Settle(context.Background(), p, req); err != nil || !s.Success || s.Extra["status"] != "mempool" {
		t.Fatalf("expected mempool settlement, got %+v %v", s, err)
	}
}

func TestKoios(t *testing.T) {
	var submitted []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer tok" {
			t.Errorf("missing token on %s", r.URL.Path)
		}
		switch r.URL.Path {
		case "/tip":
			w.Write([]byte(`[{"abs_slot":201867612,"block_time":1793433903,"epoch_no":612}]`))
		case "/epoch_params":
			if r.URL.Query().Get("_epoch_no") != "612" {
				t.Errorf("params for epoch %s", r.URL.Query().Get("_epoch_no"))
			}
			w.Write([]byte(`[{"min_fee_a":44,"min_fee_b":155381,"coins_per_utxo_size":"4310"}]`))
		case "/genesis":
			w.Write([]byte(`[{"slotlength":"1"}]`))
		case "/utxo_info":
			w.Write([]byte(`[{"tx_hash":"AB","tx_index":1,"address":"addr1x","value":"29168710","is_spent":true,
				"asset_list":[{"policy_id":"aa","asset_name":"01","quantity":"5"}]}]`))
		case "/tx_status":
			w.Write([]byte(`[{"tx_hash":"ab","num_confirmations":null}]`))
		case "/submittx":
			if r.Header.Get("Content-Type") != "application/cbor" {
				t.Errorf("submit content type %s", r.Header.Get("Content-Type"))
			}
			submitted = make([]byte, r.ContentLength)
			r.Body.Read(submitted)
			if string(submitted) == "bad" {
				http.Error(w, "BadInputsUTxO", http.StatusBadRequest)
				return
			}
			w.WriteHeader(http.StatusAccepted)
		}
	}))
	defer srv.Close()
	k := &Koios{URL: srv.URL + "/", Token: "tok"}
	ctx := context.Background()

	if tip, err := k.Tip(ctx); err != nil || tip.Slot != 201867612 || tip.Time.Unix() != 1793433903 {
		t.Fatalf("tip %+v %v", tip, err)
	}
	if p, err := k.Params(ctx); err != nil || p != (Params{MinFeeA: 44, MinFeeB: 155381, CoinsPerUTxOByte: 4310, SlotLength: time.Second}) {
		t.Fatalf("params %+v %v", p, err)
	}
	outs, err := k.Outputs(ctx, []string{"ab#1"})
	if o := outs["ab#1"]; err != nil || o.Lovelace != 29168710 || !o.Spent || o.Assets["aa01"] != 5 || o.Address != "addr1x" {
		t.Fatalf("outputs %+v %v", outs, err)
	}
	if n, err := k.Confirmations(ctx, "ab"); err != nil || n != -1 {
		t.Fatalf("confirmations %d %v", n, err)
	}
	if err := k.Submit(ctx, []byte("tx")); err != nil || string(submitted) != "tx" {
		t.Fatalf("submit %v", err)
	}
	if err, ok := k.Submit(ctx, []byte("bad")).(*Rejected); !ok || err.Reason != "BadInputsUTxO" {
		t.Fatalf("expected rejection, got %v", err)
	}
}
