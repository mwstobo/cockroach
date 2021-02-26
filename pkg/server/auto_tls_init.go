// Copyright 2021 The Cockroach Authors.
//
// Use of this software is governed by the Business Source License
// included in the file licenses/BSL.txt.
//
// As of the Change Date specified in that file, in accordance with
// the Business Source License, use of this software will be governed
// by the Apache License, Version 2.0, included in the file
// licenses/APL.txt.

// TODO(aaron-crl): This uses the CertsLocator from the security package
// Getting about half way to integration with the certificate manager
// While I'd originally hoped to decouple it completely, I realized
// it would create an even larger headache if we maintained default
// certificate locations in multiple places.

package server

import (
	"encoding/pem"
	"io/ioutil"
	"os"
	"strings"
	"time"

	"github.com/cockroachdb/cockroach/pkg/base"
	"github.com/cockroachdb/cockroach/pkg/security"
	"github.com/cockroachdb/errors"
	"github.com/cockroachdb/errors/oserror"
)

// TODO(aaron-crl): Pluck this from Config.
// Define default CA certificate lifespan of 366 days.
const caCertLifespan = time.Hour * 24 * 366

// TODO(aaron-crl): Pluck this from Config.
// Define default service certificate lifespan of 30 days.
const serviceCertLifespan = time.Hour * 24 * 30

// Service Name Strings for autogenerated certificates.
const serviceNameInterNode = "InterNode Service"
const serviceNameUserAuth = "User Auth Service"
const serviceNameSQL = "SQL Service"
const serviceNameRPC = "RPC Service"
const serviceNameUI = "UI Service"

// CertificateBundle manages the collection of certificates used by a
// CockroachDB node.
type CertificateBundle struct {
	InterNode      ServiceCertificateBundle
	UserAuth       ServiceCertificateBundle
	SQLService     ServiceCertificateBundle
	RPCService     ServiceCertificateBundle
	AdminUIService ServiceCertificateBundle
}

// ServiceCertificateBundle is a container for the CA and host node certs.
type ServiceCertificateBundle struct {
	CACertificate   []byte
	CAKey           []byte
	HostCertificate []byte // This will be blank if unused (in the user case).
	HostKey         []byte // This will be blank if unused (in the user case).
}

// Helper function to load cert and key for a service.
func (sb *ServiceCertificateBundle) loadServiceCertAndKey(
	certPath string, keyPath string,
) (err error) {
	sb.HostCertificate, err = loadCertificateFile(certPath)
	if err != nil {
		return
	}
	sb.HostKey, err = loadKeyFile(keyPath)
	if err != nil {
		return
	}
	return
}

// Helper function to load cert and key for a service CA.
func (sb *ServiceCertificateBundle) loadCACertAndKey(certPath string, keyPath string) (err error) {
	sb.CACertificate, err = loadCertificateFile(certPath)
	if err != nil {
		return
	}
	sb.CAKey, err = loadKeyFile(keyPath)
	if err != nil {
		return
	}
	return
}

// LoadUserAuthCACertAndKey loads host certificate and key from disk or fails with error.
func (sb *ServiceCertificateBundle) loadOrCreateUserAuthCACertAndKey(
	caCertPath string, caKeyPath string, initLifespan time.Duration, serviceName string,
) (err error) {
	// Attempt to load cert into ServiceCertificateBundle.
	sb.CACertificate, err = loadCertificateFile(caCertPath)
	if err != nil {
		if oserror.IsNotExist(err) {
			// Certificate not found, attempt to create both cert and key now.
			err = sb.createServiceCA(caCertPath, caKeyPath, initLifespan, serviceName)
			if err != nil {
				return err
			}

			// Both key and cert should now be populated.
			return nil
		}

		// Some error unrelated to file existence occurred.
		return err
	}

	// Load the key only if it exists.
	sb.CAKey, err = loadKeyFile(caKeyPath)
	if !oserror.IsNotExist(err) {
		// An error returned but it was not that the file didn't exist;
		// this is an error.
		return err
	}

	return nil
}

// loadOrCreateServiceCertificates will attempt to load the service cert/key
// into the service bundle.
// * If they do not exist:
//   It will attempt to load the service CA cert/key pair.
//   * If they do not exist:
//     It will generate the service CA cert/key pair.
//     It will persist these to disk and store them
//       in the ServiceCertificateBundle.
//   It will generate the service cert/key pair.
//   It will persist these to disk and store them
//     in the ServiceCertificateBundle.
func (sb *ServiceCertificateBundle) loadOrCreateServiceCertificates(
	serviceCertPath string,
	serviceKeyPath string,
	caCertPath string,
	caKeyPath string,
	initLifespan time.Duration,
	serviceName string,
	hostnames []string,
) error {
	var err error

	// Check if the service cert and key already exist, if it does return early.
	sb.HostCertificate, err = loadCertificateFile(serviceCertPath)
	if err == nil {
		// Cert file exists, now load key.
		sb.HostKey, err = loadKeyFile(serviceKeyPath)
		if err != nil {
			// Check if we failed to load the key?
			if oserror.IsNotExist(err) {
				// Cert exists but key doesn't, this is an error.
				return errors.Wrapf(err,
					"failed to load service certificate key for %q expected key at %q",
					serviceCertPath, serviceKeyPath)
			}
			return errors.Wrap(err, "something went wrong loading service key")
		}
		// Both certificate and key should be successfully loaded.
		return nil
	}

	// Niether service cert or key exist, attempt to load CA.
	sb.CACertificate, err = loadCertificateFile(caCertPath)
	if err == nil {
		// CA cert has been successfully loaded, attempt to load
		// CA key.
		sb.CAKey, err = loadKeyFile(caKeyPath)
		if err != nil {
			return errors.Wrapf(
				err, "loaded service CA cert but failed to load service CA key file: %q", caKeyPath,
			)
		}
	} else if oserror.IsNotExist(err) {
		// CA cert does not yet exist, create it and its key.
		err = sb.createServiceCA(caCertPath, caKeyPath, initLifespan, serviceName)
		if err != nil {
			return errors.Wrap(
				err, "failed to create Service CA",
			)
		}
	}

	// CA cert and key should now be loaded, create service cert and key.
	var hostCert, hostKey []byte
	hostCert, hostKey, err = security.CreateServiceCertAndKey(
		initLifespan,
		serviceName,
		hostnames,
		sb.CACertificate,
		sb.CAKey,
	)
	if err != nil {
		return errors.Wrap(
			err, "failed to create Service Cert and Key",
		)
	}

	err = writeCertificateFile(serviceCertPath, hostCert, false)
	if err != nil {
		return err
	}

	err = writeKeyFile(serviceKeyPath, hostKey, false)
	if err != nil {
		return err
	}

	return nil
}

// createServiceCA builds CA cert and key and populates them to
// ServiceCertificateBundle.
func (sb *ServiceCertificateBundle) createServiceCA(
	caCertPath string, caKeyPath string, initLifespan time.Duration, serviceName string,
) (err error) {
	sb.CACertificate, sb.CAKey, err = security.CreateCACertAndKey(initLifespan, serviceName)
	if err != nil {
		return
	}

	err = writeCertificateFile(caCertPath, sb.CACertificate, false)
	if err != nil {
		return
	}

	err = writeKeyFile(caKeyPath, sb.CAKey, false)
	if err != nil {
		return
	}

	return
}

// Simple wrapper to make it easier to store certs somewhere else later.
// TODO (aaron-crl): Put validation checks here.
func loadCertificateFile(certPath string) (cert []byte, err error) {
	cert, err = ioutil.ReadFile(certPath)
	return
}

// Simple wrapper to make it easier to store certs somewhere else later.
// TODO (aaron-crl): Put validation checks here.
func loadKeyFile(keyPath string) (key []byte, err error) {
	key, err = ioutil.ReadFile(keyPath)
	return
}

// Simple wrapper to make it easier to store certs somewhere else later.
// Unless overwrite is true, this function will error if a file alread exists
// at certFilePath.
// TODO(aaron-crl): This was lifted from 'pkg/security' and modified. It might
// make sense to refactor these calls back to 'pkg/security' rather than
// maintain these functions.
func writeCertificateFile(certFilePath string, certificatePEMBytes []byte, overwrite bool) error {
	// Validate that we are about to write a cert. And reshape for common
	// security.WritePEMToFile().
	// TODO(aaron-crl): Validate this is actually a cert.
	caCert, _ := pem.Decode(certificatePEMBytes)
	if nil == caCert {
		return errors.New("failed to parse valid PEM from certificatePEMBytes")
	}

	// TODO(aaron-crl): Add logging here.
	return security.WritePEMToFile(certFilePath, 0600, overwrite, caCert)
}

// Simple wrapper to make it easier to store certs somewhere else later.
// Unless overwrite is true, this function will error if a file alread exists
// at keyFilePath.
// TODO(aaron-crl): This was lifted from 'pkg/security' and modified. It might
// make sense to refactor these calls back to 'pkg/security' rather than
// maintain these functions.
func writeKeyFile(keyFilePath string, keyPEMBytes []byte, overwrite bool) error {
	// Validate that we are about to write a key and reshape for common
	// security.WritePEMToFile().
	// TODO(aaron-crl): Validate this is actually a key.

	keyBlock, _ := pem.Decode(keyPEMBytes)
	if keyBlock == nil {
		return errors.New("failed to parse valid PEM from certificatePEMBytes")
	}

	// TODO(aaron-crl): Add logging here.
	return security.WritePEMToFile(keyFilePath, 600, overwrite, keyBlock)
}

// InitializeFromConfig is called by the node creating certificates for the
// cluster. It uses or generates an InterNode CA to produce any missing
// unmanaged certificates. It does this base on the logic in:
// https://github.com/cockroachdb/cockroach/pull/51991
// N.B.: This function fast fails if an InterNodeHost cert/key pair are present
// as this should _never_ happen.
func (b *CertificateBundle) InitializeFromConfig(c base.Config) error {
	cl := security.MakeCertsLocator(c.SSLCertsDir)

	// First check to see if host cert is already present
	// if it is, we should fail to initialize.
	if _, err := os.Stat(cl.NodeCertPath()); err == nil {
		return errors.New(
			"interNodeHost certificate already present")
	} else if !oserror.IsNotExist(err) {
		return errors.Wrap(
			err, "interNodeHost certificate access issue")
	}

	// Start by loading or creating the InterNode certificates.
	err := b.InterNode.loadOrCreateServiceCertificates(
		cl.NodeCertPath(),
		cl.NodeKeyPath(),
		cl.CACertPath(),
		cl.CAKeyPath(),
		serviceCertLifespan,
		serviceNameInterNode,
		[]string{c.Addr, c.AdvertiseAddr},
	)
	if err != nil {
		return errors.Wrap(err,
			"failed to load or create InterNode certificates")
	}

	// Initialize User auth certificates.
	// TODO(aaron-crl): Double check that we want to do this. It seems
	// like this is covered by the interface certificates?
	err = b.UserAuth.loadOrCreateUserAuthCACertAndKey(
		cl.ClientCACertPath(),
		cl.ClientCAKeyPath(),
		caCertLifespan,
		serviceNameUserAuth,
	)
	if err != nil {
		return errors.Wrap(err,
			"failed to load or create User auth certificate(s)")
	}

	// Initialize SQLService Certs.
	err = b.SQLService.loadOrCreateServiceCertificates(
		cl.SQLServiceCertPath(),
		cl.SQLServiceKeyPath(),
		cl.SQLServiceCACertPath(),
		cl.SQLServiceCAKeyPath(),
		serviceCertLifespan,
		serviceNameSQL,
		// TODO(aaron-crl): Add RPC variable to config or SplitSQLAddr.
		[]string{c.SQLAddr, c.SQLAdvertiseAddr},
	)
	if err != nil {
		return errors.Wrap(err,
			"failed to load or create SQL service certificate(s)")
	}

	// Initialize RPCService Certs.
	err = b.RPCService.loadOrCreateServiceCertificates(
		cl.RPCServiceCertPath(),
		cl.RPCServiceKeyPath(),
		cl.RPCServiceCACertPath(),
		cl.RPCServiceCAKeyPath(),
		serviceCertLifespan,
		serviceNameRPC,
		// TODO(aaron-crl): Add RPC variable to config.
		[]string{c.SQLAddr, c.SQLAdvertiseAddr},
	)
	if err != nil {
		return errors.Wrap(err,
			"failed to load or create RPC service certificate(s)")
	}

	// Initialize AdminUIService Certs.
	err = b.AdminUIService.loadOrCreateServiceCertificates(
		cl.UICertPath(),
		cl.UIKeyPath(),
		cl.UICACertPath(),
		cl.UICAKeyPath(),
		serviceCertLifespan,
		serviceNameUI,
		[]string{c.HTTPAddr, c.HTTPAdvertiseAddr},
	)
	if err != nil {
		return errors.Wrap(err,
			"failed to load or create Admin UI service certificate(s)")
	}

	return nil
}

// InitializeNodeFromBundle uses the contents of the CertificateBundle and
// details from the config object to write certs to disk and generate any
// missing host-specific certificates and keys
// It is assumed that a node receiving this has not has TLS initialized. If
// a interNodeHost certificate is found, this function will error.
func (b *CertificateBundle) InitializeNodeFromBundle(c base.Config) error {
	cl := security.MakeCertsLocator(c.SSLCertsDir)

	// First check to see if host cert is already present
	// if it is, we should fail to initialize.
	if _, err := os.Stat(cl.NodeCertPath()); err == nil {
		return errors.New("interNodeHost certificate already present")
	} else if !oserror.IsNotExist(err) {
		// Something else went wrong accessing the path
		return err
	}

	// Write received CA's to disk. If any of them already exist, fail
	// and return an error.

	// Attempt to write InterNodeHostCA to disk first.
	err := b.InterNode.writeCAOrFail(cl.CACertPath(), cl.CAKeyPath())
	if err != nil {
		return errors.Wrap(err, "failed to write InterNodeCA to disk")
	}

	// Attempt to write ClientCA to disk.
	err = b.InterNode.writeCAOrFail(cl.ClientCACertPath(), cl.ClientCAKeyPath())
	if err != nil {
		return errors.Wrap(err, "failed to write ClientCA to disk")
	}

	// Attempt to write SQLServiceCA to disk.
	err = b.InterNode.writeCAOrFail(cl.SQLServiceCACertPath(), cl.SQLServiceCAKeyPath())
	if err != nil {
		return errors.Wrap(err, "failed to write SQLServiceCA to disk")
	}

	// Attempt to write RPCServiceCA to disk.
	err = b.InterNode.writeCAOrFail(cl.RPCServiceCACertPath(), cl.RPCServiceCAKeyPath())
	if err != nil {
		return errors.Wrap(err, "failed to write RPCServiceCA to disk")
	}

	// Attempt to write AdminUIServiceCA to disk.
	err = b.InterNode.writeCAOrFail(cl.UICACertPath(), cl.UICAKeyPath())
	if err != nil {
		return errors.Wrap(err, "failed to write AdminUIServiceCA to disk")
	}

	// Once CAs are written call the same InitFromConfig function to create
	// host certificates.
	err = b.InitializeFromConfig(c)
	if err != nil {
		return errors.Wrap(
			err,
			"failed to initialize host certs after writing CAs to disk")
	}

	return nil
}

// writeCAOrFail will attempt to write a service certificate bundle to the
// specified paths on disk. It will ignore any missing certificate fields but
// error if it fails to write a file to disk.
func (sb *ServiceCertificateBundle) writeCAOrFail(certPath string, keyPath string) (err error) {
	if sb.CACertificate != nil {
		err = writeCertificateFile(certPath, sb.CACertificate, false)
		if err != nil {
			return
		}
	}

	if sb.CAKey != nil {
		err = writeKeyFile(keyPath, sb.CAKey, false)
		if err != nil {
			return
		}
	}

	return
}

func (sb *ServiceCertificateBundle) loadCACertAndKeyIfExists(
	certPath string, keyPath string,
) error {
	// TODO(aaron-crl): Possibly add a warning to the log that a CA was not
	// found
	err := sb.loadCACertAndKey(certPath, keyPath)
	if oserror.IsNotExist(err) {
		return nil
	}
	return err
}

// collectLocalCABundle will load any CA certs and keys present on disk. It
// will skip any CA's where the certificate is not found. Any other read errors
// including permissions result in an error.
func collectLocalCABundle(c base.Config) (CertificateBundle, error) {
	cl := security.MakeCertsLocator(c.SSLCertsDir)
	var b CertificateBundle
	var err error

	err = b.InterNode.loadCACertAndKeyIfExists(cl.CACertPath(), cl.CAKeyPath())
	if err != nil {
		return b, errors.Wrap(
			err, "error loading InterNode CA cert and/or key")
	}

	err = b.UserAuth.loadCACertAndKeyIfExists(
		cl.ClientCACertPath(), cl.ClientCAKeyPath())
	if err != nil {
		return b, errors.Wrap(
			err, "error loading UserAuth CA cert and/or key")
	}

	err = b.SQLService.loadCACertAndKeyIfExists(
		cl.SQLServiceCACertPath(), cl.SQLServiceCAKeyPath())
	if err != nil {
		return b, errors.Wrap(
			err, "error loading SQL CA cert and/or key")
	}
	err = b.RPCService.loadCACertAndKeyIfExists(
		cl.RPCServiceCACertPath(), cl.RPCServiceCAKeyPath())
	if err != nil {
		return b, errors.Wrap(
			err, "error loading RPC CA cert and/or key")
	}

	err = b.AdminUIService.loadCACertAndKeyIfExists(
		cl.UICACertPath(), cl.UICAKeyPath())
	if err != nil {
		return b, errors.Wrap(
			err, "error loading AdminUI CA cert and/or key")
	}

	return b, nil
}

// rotateGeneratedCertsOnDisk will generate and replace interface certificates
// where a corresponding CA cert and key are found. This function does not
// restart any services or cause the node to restart. That must be triggered
// after this function is successfully run.
// Service certs are written as they are generated but will return on first
// error. This is not seen as harmful as the rotation command may be rerun
// manually after rotation errors are corrected without negatively impacting
// any interface. All existing interfaces will again receive a new
// certificate/key pair.
func rotateGeneratedCerts(c base.Config) error {
	cl := security.MakeCertsLocator(c.SSLCertsDir)
	var errStrings []string

	// Fail fast if we can't load the CAs.
	b, err := collectLocalCABundle(c)
	if err != nil {
		return errors.Wrap(
			err, "failed to load local CAs for certificate rotation")
	}

	// Rotate InterNode Certs.
	if b.InterNode.CACertificate != nil {
		err = b.InterNode.rotateServiceCert(
			cl.NodeCertPath(),
			cl.NodeKeyPath(),
			serviceCertLifespan,
			serviceNameInterNode,
			[]string{c.HTTPAddr, c.HTTPAdvertiseAddr},
		)
		if err != nil {
			return errors.Wrap(err, "failed to rotate InterNode cert")
		}
	}

	// TODO(aaron-crl): Should we rotate UserAuth Certs.

	// Rotate SQLService Certs.
	if b.SQLService.CACertificate != nil {
		err = b.SQLService.rotateServiceCert(
			cl.SQLServiceCertPath(),
			cl.SQLServiceKeyPath(),
			serviceCertLifespan,
			serviceNameSQL,
			[]string{c.HTTPAddr, c.HTTPAdvertiseAddr},
		)
		if err != nil {
			return errors.Wrap(err, "failed to rotate SQLService cert")
		}
	}

	// Rotate RPCService Certs.
	if b.RPCService.CACertificate != nil {
		err = b.RPCService.rotateServiceCert(
			cl.RPCServiceCertPath(),
			cl.RPCServiceKeyPath(),
			serviceCertLifespan,
			serviceNameRPC,
			[]string{c.HTTPAddr, c.HTTPAdvertiseAddr},
		)
		if err != nil {
			return errors.Wrap(err, "failed to rotate RPCService cert")
		}
	}

	// Rotate AdminUIService Certs.
	if b.AdminUIService.CACertificate != nil {
		err = b.AdminUIService.rotateServiceCert(
			cl.UICertPath(),
			cl.UIKeyPath(),
			serviceCertLifespan,
			serviceNameUI,
			[]string{c.HTTPAddr, c.HTTPAdvertiseAddr},
		)
		if err != nil {
			return errors.Wrap(err, "failed to rotate AdminUIService cert")
		}
	}

	return errors.Errorf(strings.Join(errStrings, "\n"))
}

// rotateServiceCert will generate a new service certificate for the provided
// hostnames and path signed by the ca at the supplied paths. It will only
// succeed if it is able to generate these and OVERWRITE an exist file.
func (sb *ServiceCertificateBundle) rotateServiceCert(
	certPath string,
	keyPath string,
	serviceCertLifespan time.Duration,
	serviceString string,
	hostnames []string,
) error {
	// generate
	certPEM, keyPEM, err := security.CreateServiceCertAndKey(
		serviceCertLifespan,
		serviceString,
		hostnames,
		sb.CACertificate,
		sb.CAKey,
	)
	if err != nil {
		return errors.Wrapf(
			err, "failed to rotate certs for %q", serviceString)
	}

	// Check to make sure we're about to overwrite a file.
	if _, err := os.Stat(certPath); err != nil {
		err = writeCertificateFile(certPath, certPEM, true)
		if err != nil {
			return errors.Wrapf(
				err, "failed to rotate certs for %q", serviceString)
		}
	} else {
		return errors.Wrapf(
			err, "failed to rotate certs for %q", serviceString)
	}

	// Check to make sure we're about to overwrite a file.
	if _, err := os.Stat(certPath); err != nil {
		err = writeKeyFile(keyPath, keyPEM, true)
		if err != nil {
			return errors.Wrapf(
				err, "failed to rotate certs for %q", serviceString)
		}
	} else {
		return errors.Wrapf(
			err, "failed to rotate certs for %q", serviceString)
	}

	return nil
}
