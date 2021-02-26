// Copyright 2021 The Cockroach Authors.
//
// Use of this software is governed by the Business Source License
// included in the file licenses/BSL.txt.
//
// As of the Change Date specified in that file, in accordance with
// the Business Source License, use of this software will be governed
// by the Apache License, Version 2.0, included in the file
// licenses/APL.txt.

package security

import (
	"bytes"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"net"
	"time"

	"github.com/cockroachdb/cockroach/pkg/util/timeutil"
	"github.com/cockroachdb/errors"
)

// TODO(aaron-crl): This shared a name and purpose with the value in
// pkg/security and should be consolidated.
const defaultKeySize = 4096

// notBeforeMargin provides a window to compensate for potential clock skew.
const notBeforeMargin = time.Second * 30

// createCertificateSerialNumber is a helper function that generates a
// random value between [1, 2^130). The use of crypto random for a serial with
// greater than 128 bits of entropy provides for a potential future where we
// decided to rely on the serial for security purposes.
func createCertificateSerialNumber() (serialNumber *big.Int, err error) {
	max := new(big.Int)
	max.Exp(big.NewInt(2), big.NewInt(130), nil).Sub(max, big.NewInt(1))

	// serialNumber is set using rand.Int which yields a value between [0, max)
	// where max is (2^130)-1.
	serialNumber, err = rand.Int(rand.Reader, max)
	if err != nil {
		err = errors.Wrap(err, "failed to create new serial number")
	}

	// We then add 1 to the result ensuring a range of [1,2^130).
	serialNumber.Add(serialNumber, big.NewInt(1))

	return
}

// CreateCACertAndKey will create a CA with a validity beginning
// now() and expiring after `lifespan`. This is a utility function to help
// with cluster auto certificate generation.
func CreateCACertAndKey(
	lifespan time.Duration, service string,
) (certPEM []byte, keyPEM []byte, err error) {
	notBefore := timeutil.Now().Add(-notBeforeMargin)
	notAfter := timeutil.Now().Add(lifespan)

	// Create random serial number for CA.
	serialNumber, err := createCertificateSerialNumber()
	if err != nil {
		return nil, nil, err
	}

	// Create short lived initial CA template.
	ca := &x509.Certificate{
		SerialNumber: serialNumber,
		Subject: pkix.Name{
			Organization:       []string{"Cockroach Labs"},
			OrganizationalUnit: []string{service},
			Country:            []string{"US"},
		},
		NotBefore:             notBefore,
		NotAfter:              notAfter,
		IsCA:                  true,
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		BasicConstraintsValid: true,
		MaxPathLenZero:        true,
	}

	// Create private and public key for CA.
	caPrivKey, err := rsa.GenerateKey(rand.Reader, defaultKeySize)
	if err != nil {
		return nil, nil, err
	}

	caPrivKeyPEM := new(bytes.Buffer)
	caPrivKeyPEMBytes, err := x509.MarshalPKCS8PrivateKey(caPrivKey)
	if err != nil {
		return nil, nil, err
	}

	err = pem.Encode(caPrivKeyPEM, &pem.Block{
		Type:  "RSA PRIVATE KEY",
		Bytes: caPrivKeyPEMBytes,
	})
	if err != nil {
		return nil, nil, err
	}

	// Create CA certificate then PEM encode it.
	caBytes, err := x509.CreateCertificate(rand.Reader, ca, ca, &caPrivKey.PublicKey, caPrivKey)
	if err != nil {
		return nil, nil, err
	}

	caPEM := new(bytes.Buffer)
	err = pem.Encode(caPEM, &pem.Block{
		Type:  "CERTIFICATE",
		Bytes: caBytes,
	})
	if err != nil {
		return nil, nil, err
	}

	certPEM = caPEM.Bytes()
	keyPEM = caPrivKeyPEM.Bytes()

	return certPEM, keyPEM, nil
}

// CreateServiceCertAndKey creates a cert/key pair signed by the provided CA.
// This is a utility function to help with cluster auto certificate generation.
func CreateServiceCertAndKey(
	lifespan time.Duration, service string, hostnames []string, caCertPEM []byte, caKeyPEM []byte,
) (certPEM []byte, keyPEM []byte, err error) {
	notBefore := timeutil.Now().Add(-notBeforeMargin)
	notAfter := timeutil.Now().Add(lifespan)

	// Create random serial number for CA.
	serialNumber, err := createCertificateSerialNumber()
	if err != nil {
		return nil, nil, err
	}

	caCertBlock, _ := pem.Decode(caCertPEM)
	if caCertBlock == nil {
		err = errors.New("failed to parse valid PEM from CaCertificate blob")
		return nil, nil, err
	}

	caCert, err := x509.ParseCertificate(caCertBlock.Bytes)
	if err != nil {
		err = errors.Wrap(err, "failed to parse valid Certificate from PEM blob")
		return nil, nil, err
	}

	caKeyBlock, _ := pem.Decode(caKeyPEM)
	if caKeyBlock == nil {
		err = errors.New("failed to parse valid PEM from CaKey blob")
		return nil, nil, err
	}

	caKey, err := x509.ParsePKCS8PrivateKey(caKeyBlock.Bytes)
	if err != nil {
		err = errors.Wrap(err, "failed to parse valid Certificate from PEM blob")
		return nil, nil, err
	}

	// Bulid service certificate template; template will be used for all
	// autogenerated service certificates.
	// TODO(aaron-crl): This should match the implementation in
	// pkg/security/x509.go until we can consolidate them.
	serviceCert := &x509.Certificate{
		SerialNumber: serialNumber,
		Subject: pkix.Name{
			Organization:       []string{"Cockroach Labs"},
			OrganizationalUnit: []string{service},
			Country:            []string{"US"},
		},
		NotBefore:   notBefore,
		NotAfter:    notAfter,
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		KeyUsage:    x509.KeyUsageKeyEncipherment | x509.KeyUsageDigitalSignature,
	}

	// Attempt to parse hostname as IP, if successful add it as an IP
	// otherwise presume it is a DNS name.
	// TODO(aaron-crl): Pass these values via config object.
	for _, hostname := range hostnames {
		ip := net.ParseIP(hostname)
		if ip != nil {
			serviceCert.IPAddresses = []net.IP{ip}
		} else {
			serviceCert.DNSNames = []string{hostname}
		}
	}

	servicePrivKey, err := rsa.GenerateKey(rand.Reader, defaultKeySize)
	if err != nil {
		return nil, nil, err
	}

	serviceCertBytes, err := x509.CreateCertificate(rand.Reader, serviceCert, caCert, &servicePrivKey.PublicKey, caKey)
	if err != nil {
		return nil, nil, err
	}

	serviceCertBlock := new(bytes.Buffer)
	err = pem.Encode(serviceCertBlock, &pem.Block{
		Type:  "CERTIFICATE",
		Bytes: serviceCertBytes,
	})
	if err != nil {
		return nil, nil, err
	}

	servicePrivKeyPEM := new(bytes.Buffer)
	certPrivKeyPEMBytes, err := x509.MarshalPKCS8PrivateKey(servicePrivKey)
	if err != nil {
		return nil, nil, err
	}

	err = pem.Encode(servicePrivKeyPEM, &pem.Block{
		Type:  "RSA PRIVATE KEY",
		Bytes: certPrivKeyPEMBytes,
	})
	if err != nil {
		return nil, nil, err
	}

	return serviceCertBlock.Bytes(), servicePrivKeyPEM.Bytes(), nil
}
