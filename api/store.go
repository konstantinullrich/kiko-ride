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
)

type Car struct {
	Brand              string `json:"brand"`
	Model              string `json:"model"`
	Color              string `json:"color"`
	LicensePlateNumber string `json:"license_plate_number"`
}

type Driver struct {
	ID string `json:"id"`
	// PublicKey is the driver's cosigner key, a CIP-1854 "acct_shared_xvk".
	// Its key at each ride's address index cosigns that ride, and the driver
	// is paid out by signing a spend from the ride address with it.
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
	DriverID     string
	EtaMinutes   int
	EtaKm        float64
	FareLovelace uint64

	// PaymentTx is the settled transaction once Status is StatusPaid.
	PaymentTx string
	// PayoutTx is the last payout transaction the server cosigned. The
	// driver submits it, so it may not be on chain.
	PayoutTx string
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

func (s *store) addDriver(d Driver) Driver {
	s.mu.Lock()
	defer s.mu.Unlock()
	d.ID = newID()
	s.drivers[d.ID] = &d
	return d
}

func (s *store) driver(id string) (Driver, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	d, ok := s.drivers[id]
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
	r.DriverID = d.ID
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

// approvePayout lets the ride's driver be paid out once the fare is settled,
// recording the transaction the server is about to cosign. This is where a
// rider's dispute will block the payout.
func (s *store) approvePayout(rideID, driverID, txID string) (Ride, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, ok := s.rides[rideID]
	if !ok || r.DriverID != driverID {
		return Ride{}, errNotFound
	}
	if r.Status != StatusPaid {
		return Ride{}, errConflict
	}
	r.PayoutTx = txID
	return *r, nil
}
