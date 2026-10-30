// Package bsvmultisig is the Bitcoin SV twin of package multisig: a 2-of-3
// HD multisig wallet and the Kiko server cosigner, built on the BSV Go SDK.
//
// Each cosigner keeps their own mnemonic and shares a single BIP-32 account
// extended public key ("xpub…" on mainnet, "tpub…" on testnet) once. From the
// three account keys the backend derives an unlimited number of unlinkable
// locking scripts without any private key. Index i is locked by the bare
// multisig script
//
//	OP_2 <A at m/48'/236'/acct'/0/i> <B …> <C …> OP_3 OP_CHECKMULTISIG
//
// with the three compressed public keys sorted (BIP-67), so every party
// derives the same script whatever order the keys are passed in. 236 is the
// SLIP-44 coin type of BSV; purpose 48' keeps these keys apart from the
// single-sig keys of the same mnemonic.
//
// BSV dropped P2SH in the Genesis upgrade, so the multisig script sits in
// the output itself and has no address: a ride is paid to its locking
// script.
//
// # Flow
//
//	signer, _ := bsvmultisig.NewSigner(bsvmultisig.Testnet, mnemonic, 0)
//	share := signer.AccountKey() // "tpub..."
//
//	w, _ := bsvmultisig.NewWallet(bsvmultisig.Testnet, shareA, shareB, shareC)
//	addr, _ := w.Address(7)
//
//	p, _ := w.BuildPayment(ctx, chain, bsvmultisig.Payment{To: dest, Satoshis: 50_000})
//	witA, _ := signerA.Sign(p)
//	witC, _ := signerC.Sign(p)
//	tx, _ := w.Assemble(p, append(witA, witC...)...)
//	broadcaster.BroadcastCtx(ctx, tx) // e.g. ARC
//
// Proposals carry the transaction in Extended Format (BRC-30), which
// includes each input's amount and locking script, so a cosigner can compute
// the signature hashes without looking anything up. Every signature uses
// SIGHASH_ALL|FORKID.
//
// # Kiko server cosigner
//
// Server mirrors multisig.Server: account 0 cosigns every ride wallet and
// account 1 stands in for a rider who brings no key. Server.Cosign adds the
// server's signature to a transaction the counterparty has already signed.
// Unlike Cardano, a BSV transaction id covers the unlocking scripts, so
// cosigning changes the id; Cosign returns the final one.
package bsvmultisig
