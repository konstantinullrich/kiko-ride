package api

import (
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	_ "modernc.org/sqlite"

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
}

// OpenDB opens (creating if needed) the SQLite database at path.
func OpenDB(path string) (*sql.DB, error) {
	db, err := sql.Open("sqlite", "file:"+path+"?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)")
	if err != nil {
		return nil, err
	}
	// One connection serializes writes, so the conditional updates below
	// cannot interleave.
	db.SetMaxOpenConns(1)
	if err := db.Ping(); err != nil {
		db.Close()
		return nil, fmt.Errorf("open %s: %w", path, err)
	}
	return db, nil
}

// The address index is the rides table's AUTOINCREMENT key: SQLite never
// hands out the same one twice, even after rows are deleted.
const schema = `
CREATE TABLE IF NOT EXISTS drivers (
	public_key               TEXT PRIMARY KEY,
	base_fee                 INTEGER NOT NULL,
	fee_per_km               INTEGER NOT NULL,
	car_brand                TEXT NOT NULL,
	car_model                TEXT NOT NULL,
	car_color                TEXT NOT NULL,
	car_license_plate_number TEXT NOT NULL,
	car_year                 INTEGER NOT NULL,
	created_at               TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS rides (
	address_index INTEGER PRIMARY KEY AUTOINCREMENT,
	id            TEXT NOT NULL UNIQUE,
	status        TEXT NOT NULL,
	start_address TEXT NOT NULL,
	end_address   TEXT NOT NULL,
	start_lat     REAL NOT NULL,
	start_lng     REAL NOT NULL,
	end_lat       REAL NOT NULL,
	end_lng       REAL NOT NULL,
	distance_km   REAL NOT NULL,
	pickup_time   TEXT NOT NULL,
	created_at    TEXT NOT NULL,
	rider_key     TEXT NOT NULL DEFAULT '',
	pay_to        TEXT NOT NULL DEFAULT '',
	script        TEXT NOT NULL DEFAULT '',
	driver_key    TEXT NOT NULL DEFAULT '',
	eta_minutes   INTEGER NOT NULL DEFAULT 0,
	eta_km        REAL NOT NULL DEFAULT 0,
	fare_lovelace INTEGER NOT NULL DEFAULT 0,
	payment_tx    TEXT NOT NULL DEFAULT '',
	payout_tx     TEXT NOT NULL DEFAULT '',
	-- claim_tx is the transaction being settled for the ride, so a second,
	-- different payment is refused instead of charged twice. It survives a
	-- restart: the first transaction may have settled meanwhile.
	claim_tx      TEXT NOT NULL DEFAULT '',
	-- paying_out is set while a payout is being submitted, so two payouts
	-- cannot race.
	paying_out    INTEGER NOT NULL DEFAULT 0
);

CREATE INDEX IF NOT EXISTS rides_status ON rides (status);
`

// store keeps drivers and rides in SQLite.
type store struct {
	db *sql.DB
}

func newStore(db *sql.DB) (*store, error) {
	if _, err := db.Exec(schema); err != nil {
		return nil, fmt.Errorf("create schema: %w", err)
	}
	// No payout submission outlives the process; the chain refuses a
	// duplicate of one that went through.
	if _, err := db.Exec(`UPDATE rides SET paying_out = 0 WHERE paying_out = 1`); err != nil {
		return nil, err
	}
	return &store{db: db}, nil
}

func newID() string {
	b := make([]byte, 16)
	rand.Read(b)
	return hex.EncodeToString(b)
}

const timeLayout = time.RFC3339Nano

// putDriver registers d under its public key, or replaces the profile of
// the driver already registered there; created reports which. Rides already
// accepted keep the fare fixed at acceptance.
func (s *store) putDriver(d Driver) (created bool, err error) {
	now := time.Now().UTC().Format(timeLayout)
	var createdAt string
	err = s.db.QueryRow(`INSERT INTO drivers (public_key, base_fee, fee_per_km, car_brand, car_model,
		car_color, car_license_plate_number, car_year, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (public_key) DO UPDATE SET base_fee = excluded.base_fee, fee_per_km = excluded.fee_per_km,
			car_brand = excluded.car_brand, car_model = excluded.car_model, car_color = excluded.car_color,
			car_license_plate_number = excluded.car_license_plate_number, car_year = excluded.car_year
		RETURNING created_at`,
		d.PublicKey, int64(d.BaseFee), int64(d.FeePerKm), d.Car.Brand, d.Car.Model,
		d.Car.Color, d.Car.LicensePlateNumber, d.Car.Year, now).Scan(&createdAt)
	return createdAt == now, err
}

const driverColumns = `public_key, base_fee, fee_per_km, car_brand, car_model, car_color,
	car_license_plate_number, car_year`

func scanDriver(row interface{ Scan(...any) error }) (Driver, error) {
	var d Driver
	var baseFee, feePerKm int64
	err := row.Scan(&d.PublicKey, &baseFee, &feePerKm, &d.Car.Brand, &d.Car.Model, &d.Car.Color,
		&d.Car.LicensePlateNumber, &d.Car.Year)
	d.BaseFee, d.FeePerKm = uint64(baseFee), uint64(feePerKm)
	return d, err
}

func (s *store) driver(publicKey string) (Driver, error) {
	d, err := scanDriver(s.db.QueryRow(`SELECT `+driverColumns+` FROM drivers WHERE public_key = ?`, publicKey))
	if errors.Is(err, sql.ErrNoRows) {
		return Driver{}, errNotFound
	}
	return d, err
}

func (s *store) allDrivers() ([]Driver, error) {
	rows, err := s.db.Query(`SELECT ` + driverColumns + ` FROM drivers`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Driver
	for rows.Next() {
		d, err := scanDriver(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

// addRide stores r under a new id and reserves its own address index.
func (s *store) addRide(r Ride) (Ride, error) {
	r.ID = newID()
	r.Status = StatusRequested
	var index int64
	err := s.db.QueryRow(`INSERT INTO rides (id, status, start_address, end_address, start_lat, start_lng,
		end_lat, end_lng, distance_km, pickup_time, created_at, rider_key)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?) RETURNING address_index`,
		r.ID, r.Status, r.StartAddress, r.EndAddress, r.Start.Lat, r.Start.Lng, r.End.Lat, r.End.Lng,
		r.DistanceKm, r.PickupTime.Format(timeLayout), r.CreatedAt.UTC().Format(timeLayout), r.RiderKey,
	).Scan(&index)
	if err != nil {
		return Ride{}, err
	}
	r.AddressIndex = uint32(index)
	return r, nil
}

const rideColumns = `id, status, start_address, end_address, start_lat, start_lng, end_lat, end_lng,
	distance_km, pickup_time, created_at, address_index, rider_key, pay_to, script, driver_key,
	eta_minutes, eta_km, fare_lovelace, payment_tx, payout_tx`

func scanRide(row interface{ Scan(...any) error }) (Ride, error) {
	var r Ride
	var pickup, created string
	var fareLovelace int64
	err := row.Scan(&r.ID, &r.Status, &r.StartAddress, &r.EndAddress, &r.Start.Lat, &r.Start.Lng,
		&r.End.Lat, &r.End.Lng, &r.DistanceKm, &pickup, &created, &r.AddressIndex, &r.RiderKey,
		&r.PayTo, &r.Script, &r.DriverKey, &r.EtaMinutes, &r.EtaKm, &fareLovelace, &r.PaymentTx, &r.PayoutTx)
	if err != nil {
		return Ride{}, err
	}
	r.FareLovelace = uint64(fareLovelace)
	if r.PickupTime, err = time.Parse(timeLayout, pickup); err != nil {
		return Ride{}, fmt.Errorf("ride %s pickup_time: %w", r.ID, err)
	}
	if r.CreatedAt, err = time.Parse(timeLayout, created); err != nil {
		return Ride{}, fmt.Errorf("ride %s created_at: %w", r.ID, err)
	}
	return r, nil
}

func (s *store) ride(id string) (Ride, error) {
	r, err := scanRide(s.db.QueryRow(`SELECT `+rideColumns+` FROM rides WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return Ride{}, errNotFound
	}
	return r, err
}

func (s *store) ridesWithStatus(status RideStatus) ([]Ride, error) {
	rows, err := s.db.Query(`SELECT `+rideColumns+` FROM rides WHERE status = ?`, status)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Ride
	for rows.Next() {
		r, err := scanRide(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// update runs a conditional update of one ride and reloads it. When no row
// matches it reports errNotFound if the ride is missing, otherwise
// errConflict.
func (s *store) update(id, query string, args ...any) (Ride, error) {
	res, err := s.db.Exec(query, args...)
	if err != nil {
		return Ride{}, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return Ride{}, err
	}
	r, err := s.ride(id)
	if err == nil && n == 0 {
		err = errConflict
	}
	return r, err
}

// accept assigns a requested ride to a driver, to be paid to payTo.
func (s *store) accept(rideID string, d Driver, payTo, script string, etaMinutes int, etaKm float64) (Ride, error) {
	r, err := s.ride(rideID)
	if err != nil {
		return Ride{}, err
	}
	return s.update(rideID, `UPDATE rides SET status = ?, pay_to = ?, script = ?, driver_key = ?,
		eta_minutes = ?, eta_km = ?, fare_lovelace = ? WHERE id = ? AND status = ?`,
		StatusAccepted, payTo, script, d.PublicKey, etaMinutes, etaKm, int64(fare(d, r.DistanceKm)),
		rideID, StatusRequested)
}

// claim reserves an accepted ride for settlement by txID. The same
// transaction may claim again (settlement retries); another gets
// errConflict.
func (s *store) claim(rideID, txID string) (Ride, error) {
	return s.update(rideID, `UPDATE rides SET claim_tx = ?
		WHERE id = ? AND status = ? AND claim_tx IN ('', ?)`,
		txID, rideID, StatusAccepted, txID)
}

// finishClaim marks the ride paid when settled, otherwise frees the claim.
func (s *store) finishClaim(rideID, txID string, settled bool) error {
	var err error
	if settled {
		_, err = s.db.Exec(`UPDATE rides SET claim_tx = '', status = ?, payment_tx = ?
			WHERE id = ? AND claim_tx = ?`, StatusPaid, txID, rideID, txID)
	} else {
		_, err = s.db.Exec(`UPDATE rides SET claim_tx = '' WHERE id = ? AND claim_tx = ?`, rideID, txID)
	}
	return err
}

// beginPayout lets the ride's driver be paid out once the fare is settled
// and no other payout is in flight. This is where a rider's dispute will
// block the payout.
func (s *store) beginPayout(rideID, driverKey string) error {
	r, err := s.update(rideID, `UPDATE rides SET paying_out = 1
		WHERE id = ? AND driver_key = ? AND status = ? AND paying_out = 0`,
		rideID, driverKey, StatusPaid)
	if errors.Is(err, errConflict) && r.DriverKey != driverKey {
		return errNotFound
	}
	return err
}

// finishPayout marks the ride paid out when submitted, otherwise lets the
// driver try again.
func (s *store) finishPayout(rideID, txID string, submitted bool) (Ride, error) {
	if submitted {
		_, err := s.db.Exec(`UPDATE rides SET paying_out = 0, status = ?, payout_tx = ? WHERE id = ?`,
			StatusPaidOut, txID, rideID)
		if err != nil {
			return Ride{}, err
		}
	} else if _, err := s.db.Exec(`UPDATE rides SET paying_out = 0 WHERE id = ?`, rideID); err != nil {
		return Ride{}, err
	}
	return s.ride(rideID)
}
