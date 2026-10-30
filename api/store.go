package api

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"sync"
	"time"

	"kiko-backend/geo"
)

var (
	errNotFound = errors.New("not found")
	errConflict = errors.New("conflict")
)

// RideStatus is where a ride is in its lifecycle.
type RideStatus string

const (
	// StatusRequested: created by a rider, waiting for a driver.
	StatusRequested RideStatus = "requested"
	// StatusAccepted: a driver took it; the rider has not paid yet.
	StatusAccepted RideStatus = "accepted"
	// StatusPaid: the fare is settled on chain.
	StatusPaid RideStatus = "paid"
	// StatusPaidOut: the server cosigned and submitted the driver's payout.
	StatusPaidOut RideStatus = "paid_out"
)

type Car struct {
	Brand              string `json:"brand"`
	Model              string `json:"model"`
	Color              string `json:"color"`
	LicensePlateNumber string `json:"license_plate_number"`
	// Year is the model year.
	Year int `json:"year"`
}

type Driver struct {
	// PublicKey identifies the driver and is their cosigner key, a CIP-1854
	// "acct_shared_xvk". Its key at each ride's address index cosigns that
	// ride, and the driver is paid out by signing a spend from the ride
	// address with it.
	PublicKey string `json:"public_key"`
	// BaseFee and FeePerKm are in lovelace.
	BaseFee  uint64 `json:"base_fee"`
	FeePerKm uint64 `json:"fee_per_km"`
	Car      Car    `json:"car"`
}

type Ride struct {
	ID           string
	StartAddress string
	EndAddress   string
	Start, End   geo.Point
	DistanceKm   float64
	PickupTime   time.Time
	Status       RideStatus
	CreatedAt    time.Time

	// AddressIndex is reserved for this ride; every cosigner's key for the
	// ride is derived at it. Script is the hex CBOR native script locking
	// PayTo, which the driver attaches to the payout.
	AddressIndex uint32
	// RiderKey is the rider's "acct_shared_xvk", or empty if the server's
	// backup key stands in for the rider.
	RiderKey string

	// Set once a driver accepts. PayTo is the 2-of-3 address of the server,
	// the driver and the rider; the fare is paid there.
	PayTo        string
	Script       string
	DriverKey    string
	EtaMinutes   int
	EtaKm        float64
	FareLovelace uint64

	// PaymentTx is the settled transaction once Status is StatusPaid.
	PaymentTx string
	// PayoutTx is the submitted payout once Status is StatusPaidOut.
	PayoutTx string
	// payingOut is set while a payout is being submitted, so two payouts
	// cannot race.
	payingOut bool
	// claimTx is the transaction currently being settled for this ride, so a
	// second, different payment is refused instead of charged twice.
	claimTx string
}

// store keeps drivers and rides in memory.
type store struct {
	mu        sync.Mutex
	drivers   map[string]*Driver
	rides     map[string]*Ride
	nextIndex uint32
}

func newStore(firstIndex uint32) *store {
	return &store{drivers: map[string]*Driver{}, rides: map[string]*Ride{}, nextIndex: firstIndex}
}

func newID() string {
	b := make([]byte, 16)
	rand.Read(b)
	return hex.EncodeToString(b)
}

// addDriver registers d under its public key, which must be new.
func (s *store) addDriver(d Driver) (Driver, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.drivers[d.PublicKey]; ok {
		return Driver{}, errConflict
	}
	s.drivers[d.PublicKey] = &d
	return d, nil
}

func (s *store) driver(publicKey string) (Driver, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	d, ok := s.drivers[publicKey]
	if !ok {
		return Driver{}, false
	}
	return *d, true
}

func (s *store) allDrivers() []Driver {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Driver, 0, len(s.drivers))
	for _, d := range s.drivers {
		out = append(out, *d)
	}
	return out
}

// addRide stores r under a new id and reserves its own address index.
func (s *store) addRide(r Ride) Ride {
	s.mu.Lock()
	defer s.mu.Unlock()
	r.ID = newID()
	r.Status = StatusRequested
	r.AddressIndex = s.nextIndex
	s.nextIndex++
	s.rides[r.ID] = &r
	return r
}

func (s *store) ride(id string) (Ride, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, ok := s.rides[id]
	if !ok {
		return Ride{}, false
	}
	return *r, true
}

func (s *store) ridesWithStatus(status RideStatus) []Ride {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []Ride
	for _, r := range s.rides {
		if r.Status == status {
			out = append(out, *r)
		}
	}
	return out
}

// accept assigns a requested ride to a driver, to be paid to payTo.
func (s *store) accept(rideID string, d Driver, payTo, script string, etaMinutes int, etaKm float64) (Ride, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, ok := s.rides[rideID]
	if !ok {
		return Ride{}, errNotFound
	}
	if r.Status != StatusRequested {
		return Ride{}, errConflict
	}
	r.Status = StatusAccepted
	r.PayTo = payTo
	r.Script = script
	r.DriverKey = d.PublicKey
	r.EtaMinutes = etaMinutes
	r.EtaKm = etaKm
	r.FareLovelace = fare(d, r.DistanceKm)
	return *r, nil
}

// claim reserves an accepted ride for settlement by txID. The same
// transaction may claim again (settlement retries); another may not.
func (s *store) claim(rideID, txID string) (Ride, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, ok := s.rides[rideID]
	if !ok || r.Status != StatusAccepted || (r.claimTx != "" && r.claimTx != txID) {
		return Ride{}, false
	}
	r.claimTx = txID
	return *r, true
}

// finishClaim marks the ride paid when settled, otherwise frees the claim.
func (s *store) finishClaim(rideID, txID string, settled bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, ok := s.rides[rideID]
	if !ok || r.claimTx != txID {
		return
	}
	r.claimTx = ""
	if settled {
		r.Status = StatusPaid
		r.PaymentTx = txID
	}
}

// beginPayout lets the ride's driver be paid out once the fare is settled
// and no other payout is in flight. This is where a rider's dispute will
// block the payout.
func (s *store) beginPayout(rideID, driverKey string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, ok := s.rides[rideID]
	if !ok || r.DriverKey != driverKey {
		return errNotFound
	}
	if r.Status != StatusPaid || r.payingOut {
		return errConflict
	}
	r.payingOut = true
	return nil
}

// finishPayout marks the ride paid out when submitted, otherwise lets the
// driver try again.
func (s *store) finishPayout(rideID, txID string, submitted bool) Ride {
	s.mu.Lock()
	defer s.mu.Unlock()
	r := s.rides[rideID]
	r.payingOut = false
	if submitted {
		r.Status = StatusPaidOut
		r.PayoutTx = txID
	}
	return *r
}
