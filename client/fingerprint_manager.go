package client

import (
	"log"
	"strings"
	"sync"
	"time"

	"github.com/hashicorp/nomad/client/config"
	"github.com/hashicorp/nomad/client/driver"
	"github.com/hashicorp/nomad/client/fingerprint"
	cstructs "github.com/hashicorp/nomad/client/structs"
	"github.com/hashicorp/nomad/nomad/structs"
)

// FingerprintManager runs a client fingerprinters on a continuous basis, and
// updates the client when the node has changed
type FingerprintManager struct {
	getConfig  func() *config.Config
	node       *structs.Node
	nodeLock   sync.Mutex
	shutdownCh chan struct{}

	// updateNodeAttributes is a callback to the client to update the state of its
	// associated node
	updateNodeAttributes func(*cstructs.FingerprintResponse) *structs.Node

	// updateNodeFromDriver is a callback to the client to update the state of a
	// specific driver for the node
	updateNodeFromDriver func(string, *structs.DriverInfo, *structs.DriverInfo) *structs.Node
	logger               *log.Logger
}

// NewFingerprintManager is a constructor that creates and returns an instance
// of FingerprintManager
func NewFingerprintManager(getConfig func() *config.Config,
	node *structs.Node,
	shutdownCh chan struct{},
	updateNodeAttributes func(*cstructs.FingerprintResponse) *structs.Node,
	updateNodeFromDriver func(string, *structs.DriverInfo, *structs.DriverInfo) *structs.Node,
	logger *log.Logger) *FingerprintManager {
	return &FingerprintManager{
		getConfig:            getConfig,
		updateNodeAttributes: updateNodeAttributes,
		updateNodeFromDriver: updateNodeFromDriver,
		node:                 node,
		shutdownCh:           shutdownCh,
		logger:               logger,
	}
}

// Run starts the process of fingerprinting the node. It does an initial pass,
// identifying whitelisted and blacklisted fingerprints/drivers. Then, for
// those which require periotic checking, it starts a periodic process for
// each.
func (fp *FingerprintManager) Run() error {
	// First, set up all fingerprints
	cfg := fp.getConfig()
	whitelistFingerprints := cfg.ReadStringListToMap("fingerprint.whitelist")
	whitelistFingerprintsEnabled := len(whitelistFingerprints) > 0
	blacklistFingerprints := cfg.ReadStringListToMap("fingerprint.blacklist")

	fp.logger.Printf("[DEBUG] client.fingerprint_manager: built-in fingerprints: %v", fingerprint.BuiltinFingerprints())

	var availableFingerprints []string
	var skippedFingerprints []string
	for _, name := range fingerprint.BuiltinFingerprints() {
		// Skip modules that are not in the whitelist if it is enabled.
		if _, ok := whitelistFingerprints[name]; whitelistFingerprintsEnabled && !ok {
			skippedFingerprints = append(skippedFingerprints, name)
			continue
		}
		// Skip modules that are in the blacklist
		if _, ok := blacklistFingerprints[name]; ok {
			skippedFingerprints = append(skippedFingerprints, name)
			continue
		}

		availableFingerprints = append(availableFingerprints, name)
	}

	if err := fp.setupFingerprinters(availableFingerprints); err != nil {
		return err
	}

	if len(skippedFingerprints) != 0 {
		fp.logger.Printf("[DEBUG] client.fingerprint_manager: fingerprint modules skipped due to white/blacklist: %v", skippedFingerprints)
	}

	// Next, set up drivers
	// Build the white/blacklists of drivers.
	whitelistDrivers := cfg.ReadStringListToMap("driver.whitelist")
	whitelistDriversEnabled := len(whitelistDrivers) > 0
	blacklistDrivers := cfg.ReadStringListToMap("driver.blacklist")

	var availDrivers []string
	var skippedDrivers []string

	for name := range driver.BuiltinDrivers {
		// Skip fingerprinting drivers that are not in the whitelist if it is
		// enabled.
		if _, ok := whitelistDrivers[name]; whitelistDriversEnabled && !ok {
			skippedDrivers = append(skippedDrivers, name)
			continue
		}
		// Skip fingerprinting drivers that are in the blacklist
		if _, ok := blacklistDrivers[name]; ok {
			skippedDrivers = append(skippedDrivers, name)
			continue
		}

		availDrivers = append(availDrivers, name)
	}

	if err := fp.setupDrivers(availDrivers); err != nil {
		return err
	}

	if len(skippedDrivers) > 0 {
		fp.logger.Printf("[DEBUG] client.fingerprint_manager: drivers skipped due to white/blacklist: %v", skippedDrivers)
	}
	return nil
}

// setupFingerprints is used to fingerprint the node to see if these attributes are
// supported
func (fm *FingerprintManager) setupFingerprinters(fingerprints []string) error {
	var appliedFingerprints []string

	for _, name := range fingerprints {
		f, err := fingerprint.NewFingerprint(name, fm.logger)

		if err != nil {
			fm.logger.Printf("[ERR] client.fingerprint_manager: fingerprinting for %v failed: %+v", name, err)
			return err
		}

		detected, err := fm.fingerprint(name, f)
		if err != nil {
			return err
		}

		// log the fingerprinters which have been applied
		if detected {
			appliedFingerprints = append(appliedFingerprints, name)
		}

		p, period := f.Periodic()
		if p {
			go fm.runFingerprint(f, period, name)
		}
	}

	fm.logger.Printf("[DEBUG] client.fingerprint_manager: detected fingerprints %v", appliedFingerprints)
	return nil
}

// setupDrivers is used to fingerprint the node to see if these drivers are
// supported
func (fm *FingerprintManager) setupDrivers(drivers []string) error {
	var availDrivers []string
	driverCtx := driver.NewDriverContext("", "", fm.getConfig(), fm.node, fm.logger, nil)
	for _, name := range drivers {

		d, err := driver.NewDriver(name, driverCtx)
		if err != nil {
			return err
		}

		detected, err := fm.fingerprintDriver(name, d)
		if err != nil {
			fm.logger.Printf("[DEBUG] client.fingerprint_manager: fingerprinting driver %v failed: %+v", name, err)
			return err
		}

		// For all drivers without health checking enabled , create a driver
		// info which matches its fingerprint status. Later, for drivers that
		// have the health check interface implemented, a periodic health check
		// will be run
		if _, isHealthCheck := d.(fingerprint.HealthCheck); !isHealthCheck {
			healthInfo := &structs.DriverInfo{
				Healthy:    detected,
				UpdateTime: time.Now(),
			}
			if node := fm.updateNodeFromDriver(name, nil, healthInfo); node != nil {
				fm.nodeLock.Lock()
				fm.node = node
				fm.nodeLock.Unlock()
			}
		}

		// Start a periodic watcher to detect changes to a drivers health and
		// attributes.
		go fm.watchDriver(d, name)

		// Log the fingerprinters which have been applied
		if detected {
			availDrivers = append(availDrivers, name)
		}
	}

	fm.logger.Printf("[DEBUG] client.fingerprint_manager: detected drivers %v", availDrivers)
	return nil
}

// runFingerprint runs each fingerprinter individually on an ongoing basis
func (fm *FingerprintManager) runFingerprint(f fingerprint.Fingerprint, period time.Duration, name string) {
	fm.logger.Printf("[DEBUG] client.fingerprint_manager: fingerprinting %s every %v", name, period)

	timer := time.NewTimer(period)
	defer timer.Stop()

	for {
		select {
		case <-timer.C:
			timer.Reset(period)

			_, err := fm.fingerprint(name, f)
			if err != nil {
				fm.logger.Printf("[DEBUG] client.fingerprint_manager: periodic fingerprinting for %v failed: %+v", name, err)
				continue
			}

		case <-fm.shutdownCh:
			return
		}
	}
}

// fingerprint does an initial fingerprint of the client. If the fingerprinter
// is meant to be run continuously, a process is launched to perform this
// fingerprint on an ongoing basis in the background.
func (fm *FingerprintManager) fingerprint(name string, f fingerprint.Fingerprint) (bool, error) {
	var response cstructs.FingerprintResponse

	fm.nodeLock.Lock()
	request := &cstructs.FingerprintRequest{Config: fm.getConfig(), Node: fm.node}
	err := f.Fingerprint(request, &response)
	fm.nodeLock.Unlock()

	if err != nil {
		return false, err
	}

	if node := fm.updateNodeAttributes(&response); node != nil {
		fm.nodeLock.Lock()
		fm.node = node
		fm.nodeLock.Unlock()
	}

	return response.Detected, nil
}

// watchDrivers facilitates the different periods between fingerprint and
// health checking a driver
func (fm *FingerprintManager) watchDriver(d driver.Driver, name string) {
	var fingerprintTicker, healthTicker <-chan time.Time

	// Determine whether the fingerprinter is periodic and health checking
	isPeriodic, fingerprintPeriod := d.Periodic()
	hc, isHealthCheck := d.(fingerprint.HealthCheck)

	// Nothing to do since the state of this driver will never change
	if !isPeriodic && !isHealthCheck {
		return
	}

	// Setup the required tickers
	if isPeriodic {
		ticker := time.NewTicker(fingerprintPeriod)
		fingerprintTicker = ticker.C
		defer ticker.Stop()
		fm.logger.Printf("[DEBUG] client.fingerprint_manager: fingerprinting driver %s every %v", name, fingerprintPeriod)
	}
	if isHealthCheck {
		// Determine the interval at which to health check
		req := &cstructs.HealthCheckIntervalRequest{}
		var resp cstructs.HealthCheckIntervalResponse

		if err := hc.GetHealthCheckInterval(req, &resp); err != nil {
			fm.logger.Printf("[ERR] client.fingerprint_manager: error getting health check interval for driver %s: %v", name, err)
		} else if resp.Eligible {
			ticker := time.NewTicker(resp.Period)
			healthTicker = ticker.C
			defer ticker.Stop()
			fm.logger.Printf("[DEBUG] client.fingerprint_manager: health checking driver %s every %v", name, resp.Period)
		}
	}

	for {
		select {
		case <-fm.shutdownCh:
			return
		case <-fingerprintTicker:
			if _, err := fm.fingerprintDriver(name, d); err != nil {
				fm.logger.Printf("[DEBUG] client.fingerprint_manager: periodic fingerprinting for driver %v failed: %+v", name, err)
			}
		case <-healthTicker:
			// Determine if we should run the health check
			fm.nodeLock.Lock()
			driver, detected := fm.node.Drivers[name]
			if detected && driver != nil {
				detected = driver.Detected
			}
			fm.nodeLock.Unlock()

			if detected {
				if err := fm.runDriverHealthCheck(name, hc); err != nil {
					fm.logger.Printf("[DEBUG] client.fingerprint_manager: health checking for %v failed: %v", name, err)
				}
			}
		}
	}
}

// fingerprintDriver is a temporary solution to move towards DriverInfo and
// away from annotating a node's attributes to demonstrate support for a
// particular driver. Takes the FingerprintResponse and converts it to the
// proper DriverInfo update and then sets the prefix attributes as well
func (fm *FingerprintManager) fingerprintDriver(name string, f fingerprint.Fingerprint) (bool, error) {
	var response cstructs.FingerprintResponse

	fm.nodeLock.Lock()
	request := &cstructs.FingerprintRequest{Config: fm.getConfig(), Node: fm.node}
	err := f.Fingerprint(request, &response)
	fm.nodeLock.Unlock()

	if err != nil {
		return false, err
	}

	if node := fm.updateNodeAttributes(&response); node != nil {
		fm.nodeLock.Lock()
		fm.node = node
		fm.nodeLock.Unlock()
	}

	// COMPAT: Remove in 0.9: As of Nomad 0.8 there is a temporary measure to
	// update all driver attributes to its corresponding driver info object,
	// as eventually all drivers will need to
	// support this. Doing this so that we can enable this iteratively and also
	// in a backwards compatible way, where node attributes for drivers will
	// eventually be phased out.
	strippedAttributes := make(map[string]string, 0)
	for k, v := range response.Attributes {
		copy := k
		strings.Replace(copy, "driver.", "", 1)
		strippedAttributes[k] = v
	}

	di := &structs.DriverInfo{
		Attributes: strippedAttributes,
		Detected:   response.Detected,
	}
	if node := fm.updateNodeFromDriver(name, di, nil); node != nil {
		fm.nodeLock.Lock()
		fm.node = node
		fm.nodeLock.Unlock()
	}

	return response.Detected, nil
}

// runDriverHealthCheck checks the health of the specified resource.
func (fm *FingerprintManager) runDriverHealthCheck(name string, hc fingerprint.HealthCheck) error {
	request := &cstructs.HealthCheckRequest{}
	var response cstructs.HealthCheckResponse
	if err := hc.HealthCheck(request, &response); err != nil {
		return err
	}

	// Update the status of the node irregardless if there was an error- in the
	// case of periodic health checks, an error will occur if a health check
	// fails
	if node := fm.updateNodeFromDriver(name, nil, response.Drivers[name]); node != nil {
		fm.nodeLock.Lock()
		fm.node = node
		fm.nodeLock.Unlock()
	}

	return nil
}
