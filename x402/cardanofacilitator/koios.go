package cardanofacilitator

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Koios is a Chain backed by a Koios instance.
type Koios struct {
	// URL is the API base, e.g. "https://api.koios.rest/api/v1".
	URL string
	// Token is an optional bearer token.
	Token string
	// Client defaults to one with a 30 second timeout.
	Client *http.Client

	mu     sync.Mutex
	epoch  uint64
	params *Params
}

type koiosTip struct {
	AbsSlot   number `json:"abs_slot"`
	BlockTime number `json:"block_time"`
	EpochNo   number `json:"epoch_no"`
}

func (k *Koios) tip(ctx context.Context) (koiosTip, error) {
	var rows []koiosTip
	if err := k.call(ctx, http.MethodGet, "/tip?select=abs_slot,block_time,epoch_no", nil, &rows); err != nil {
		return koiosTip{}, err
	}
	if len(rows) != 1 {
		return koiosTip{}, fmt.Errorf("koios tip: %d rows", len(rows))
	}
	return rows[0], nil
}

func (k *Koios) Tip(ctx context.Context) (Tip, error) {
	t, err := k.tip(ctx)
	if err != nil {
		return Tip{}, err
	}
	return Tip{Slot: uint64(t.AbsSlot), Time: time.Unix(int64(t.BlockTime), 0)}, nil
}

// Params returns the current epoch's parameters, fetched once per epoch.
func (k *Koios) Params(ctx context.Context) (Params, error) {
	t, err := k.tip(ctx)
	if err != nil {
		return Params{}, err
	}
	k.mu.Lock()
	if k.params != nil && k.epoch == uint64(t.EpochNo) {
		defer k.mu.Unlock()
		return *k.params, nil
	}
	k.mu.Unlock()

	var params []struct {
		MinFeeA          number `json:"min_fee_a"`
		MinFeeB          number `json:"min_fee_b"`
		CoinsPerUTxOSize number `json:"coins_per_utxo_size"`
	}
	path := "/epoch_params?_epoch_no=" + strconv.FormatUint(uint64(t.EpochNo), 10) + "&select=min_fee_a,min_fee_b,coins_per_utxo_size"
	if err := k.call(ctx, http.MethodGet, path, nil, &params); err != nil {
		return Params{}, err
	}
	var genesis []struct {
		SlotLength number `json:"slotlength"`
	}
	if err := k.call(ctx, http.MethodGet, "/genesis?select=slotlength", nil, &genesis); err != nil {
		return Params{}, err
	}
	if len(params) != 1 || len(genesis) != 1 || params[0].CoinsPerUTxOSize == 0 || genesis[0].SlotLength == 0 {
		return Params{}, fmt.Errorf("koios: no parameters for epoch %d", t.EpochNo)
	}
	p := Params{
		MinFeeA:          uint64(params[0].MinFeeA),
		MinFeeB:          uint64(params[0].MinFeeB),
		CoinsPerUTxOByte: uint64(params[0].CoinsPerUTxOSize),
		SlotLength:       time.Duration(genesis[0].SlotLength) * time.Second,
	}
	k.mu.Lock()
	k.epoch, k.params = uint64(t.EpochNo), &p
	k.mu.Unlock()
	return p, nil
}

func (k *Koios) Outputs(ctx context.Context, refs []string) (map[string]Output, error) {
	var rows []struct {
		TxHash    string `json:"tx_hash"`
		TxIndex   number `json:"tx_index"`
		Address   string `json:"address"`
		Value     number `json:"value"`
		IsSpent   bool   `json:"is_spent"`
		AssetList []struct {
			PolicyID  string `json:"policy_id"`
			AssetName string `json:"asset_name"`
			Quantity  number `json:"quantity"`
		} `json:"asset_list"`
	}
	body := map[string]any{"_utxo_refs": refs, "_extended": true}
	if err := k.call(ctx, http.MethodPost, "/utxo_info", body, &rows); err != nil {
		return nil, err
	}
	out := make(map[string]Output, len(rows))
	for _, r := range rows {
		o := Output{Address: r.Address, Lovelace: uint64(r.Value), Spent: r.IsSpent, Assets: map[string]uint64{}}
		for _, a := range r.AssetList {
			o.Assets[strings.ToLower(a.PolicyID+a.AssetName)] += uint64(a.Quantity)
		}
		out[strings.ToLower(r.TxHash)+"#"+strconv.FormatUint(uint64(r.TxIndex), 10)] = o
	}
	return out, nil
}

// Submit posts the transaction to /submittx; Koios answers 400 when the
// node refuses it.
func (k *Koios) Submit(ctx context.Context, tx []byte) error {
	req, err := k.request(ctx, http.MethodPost, "/submittx", bytes.NewReader(tx))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/cbor")
	resp, err := k.client().Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	msg, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	switch {
	case resp.StatusCode == http.StatusBadRequest:
		return &Rejected{Reason: strings.TrimSpace(string(msg))}
	case resp.StatusCode/100 != 2:
		return fmt.Errorf("koios /submittx: status %d: %s", resp.StatusCode, bytes.TrimSpace(msg))
	}
	return nil
}

func (k *Koios) Confirmations(ctx context.Context, txID string) (int, error) {
	var rows []struct {
		NumConfirmations *number `json:"num_confirmations"`
	}
	if err := k.call(ctx, http.MethodPost, "/tx_status", map[string]any{"_tx_hashes": []string{txID}}, &rows); err != nil {
		return 0, err
	}
	if len(rows) != 1 || rows[0].NumConfirmations == nil {
		return -1, nil
	}
	return int(*rows[0].NumConfirmations), nil
}

func (k *Koios) client() *http.Client {
	if k.Client != nil {
		return k.Client
	}
	return &http.Client{Timeout: 30 * time.Second}
}

func (k *Koios) request(ctx context.Context, method, path string, body io.Reader) (*http.Request, error) {
	req, err := http.NewRequestWithContext(ctx, method, strings.TrimRight(k.URL, "/")+path, body)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	if k.Token != "" {
		req.Header.Set("Authorization", "Bearer "+k.Token)
	}
	return req, nil
}

func (k *Koios) call(ctx context.Context, method, path string, body, out any) error {
	var reader io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return err
		}
		reader = bytes.NewReader(raw)
	}
	req, err := k.request(ctx, method, path, reader)
	if err != nil {
		return err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := k.client().Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
		return fmt.Errorf("koios %s: status %d: %s", strings.SplitN(path, "?", 2)[0], resp.StatusCode, bytes.TrimSpace(msg))
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 8<<20)).Decode(out); err != nil {
		return fmt.Errorf("koios %s: %w", strings.SplitN(path, "?", 2)[0], err)
	}
	return nil
}

// number reads a non-negative integer Koios sends as a JSON number or a
// string.
type number uint64

func (n *number) UnmarshalJSON(b []byte) error {
	if string(b) == "null" {
		return nil
	}
	s := strings.Trim(string(b), `"`)
	v, err := strconv.ParseUint(s, 10, 64)
	if err != nil {
		return errors.New("koios: not a non-negative integer: " + s)
	}
	*n = number(v)
	return nil
}
