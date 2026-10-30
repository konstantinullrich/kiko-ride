package multisig

import (
	"slices"

	"github.com/blinklabs-io/gouroboros/ledger/common"
)

// scriptKeyHashes returns every key hash a native script mentions.
func scriptKeyHashes(ns common.NativeScript) []common.Blake2b224 {
	switch s := ns.Item().(type) {
	case *common.NativeScriptPubkey:
		return []common.Blake2b224{common.NewBlake2b224(s.Hash)}
	case *common.NativeScriptAll:
		return childKeyHashes(s.Scripts)
	case *common.NativeScriptAny:
		return childKeyHashes(s.Scripts)
	case *common.NativeScriptNofK:
		return childKeyHashes(s.Scripts)
	}
	return nil
}

func childKeyHashes(scripts []common.NativeScript) []common.Blake2b224 {
	var hashes []common.Blake2b224
	for _, child := range scripts {
		hashes = append(hashes, scriptKeyHashes(child)...)
	}
	return hashes
}

// satisfyingKeys returns a minimal-effort set of signed keys that satisfies
// the script, taking only as many signatures as each threshold needs.
// Time-lock scripts are not used by this wallet and never satisfy.
func satisfyingKeys(ns common.NativeScript, signed func(common.Blake2b224) bool) ([]common.Blake2b224, bool) {
	switch s := ns.Item().(type) {
	case *common.NativeScriptPubkey:
		hash := common.NewBlake2b224(s.Hash)
		return []common.Blake2b224{hash}, signed(hash)
	case *common.NativeScriptAll:
		return firstN(s.Scripts, int64(len(s.Scripts)), signed)
	case *common.NativeScriptAny:
		return firstN(s.Scripts, 1, signed)
	case *common.NativeScriptNofK:
		return firstN(s.Scripts, s.N, signed)
	}
	return nil, false
}

func firstN(scripts []common.NativeScript, n int64, signed func(common.Blake2b224) bool) ([]common.Blake2b224, bool) {
	var keys []common.Blake2b224
	var met int64
	for _, child := range scripts {
		if met >= n {
			break
		}
		if childKeys, ok := satisfyingKeys(child, signed); ok {
			for _, k := range childKeys {
				if !slices.Contains(keys, k) {
					keys = append(keys, k)
				}
			}
			met++
		}
	}
	return keys, met >= n
}
