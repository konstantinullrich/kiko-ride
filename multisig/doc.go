// Package multisig implements a 2-of-3 Cardano HD multisig wallet following
// CIP-1854, built on Apollo for transaction building.
//
// Each cosigner keeps their own mnemonic and shares a single account-level
// extended public key (bech32 "acct_shared_xvk") once. From the three account
// keys the backend can derive an unlimited number of unlinkable addresses
// without any private key: address i locks funds behind the native script
//
//	atLeast 2 [ sig(A at m/1854'/1815'/acct'/0/i), sig(B at .../0/i), sig(C at .../0/i) ]
//
// Any two cosigners can spend; losing one key does not lock the funds.
// Addresses are enterprise addresses (no stake part).
//
// # Flow
//
//	// Once, on each cosigner's device:
//	signer, _ := multisig.NewSigner(mnemonic, 0)
//	share := signer.AccountKey() // "acct_shared_xvk1..."
//
//	// On the backend:
//	w, _ := multisig.NewWallet(multisig.Preprod, shareA, shareB, shareC)
//	addr, _ := w.Address(7) // deposit address #7
//
//	// Spending: build, have any two cosigners sign, submit.
//	p, _ := w.BuildPayment(ctx, chain, multisig.Payment{To: dest, Lovelace: 5_000_000})
//	witA, _ := signerA.Sign(p) // on A's device
//	witC, _ := signerC.Sign(p) // on C's device
//	txID, _ := w.Submit(ctx, chain, p, append(witA, witC...)...)
//
// # Kiko server cosigner
//
// Server is the backend's own cosigner. RideWallet combines its key with the
// driver's and the rider's account keys; a rider without a key is replaced by
// the server's backup key (a second account from the same mnemonic), giving
// the server two of the three keys. Server.Sign signs with every server key a
// proposal's scripts contain, and Payment.From spends a ride's address index
// directly instead of discovering from index 0.
//
// Proposals and witnesses are plain bytes so they can be passed between
// parties as hex or JSON.
package multisig
