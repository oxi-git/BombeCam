// Package main implements bombecam-certgen — a utility to generate local CA and TLS Server certificates with required SANs for Mosquitto MQTTS.
package main

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha1"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"flag"
	"fmt"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"strings"
	"time"
)

var (
	defaultDNSSANs = []string{
		// Observed control endpoint for current WS03 firmware (MQTT over TLS :8883).
		"mqtts02-us.osaio.net",
		"m1-us.iotbing.com",
		"a1-us.iotbing.com",
		"iot.us-east-1.amazonaws.com",
		"localhost",
		"wss-us.osaio.net",
		// EU region: the same certificate covers both, so a local TLS
		// interceptor works whichever region the cameras use.
		"mqtts-eu.osaio.net",
		"mqtts01-eu.osaio.net",
		"wss-eu.osaio.net",
		"ali-wss-eu.osaio.net",
		"bombecam-mosquitto",
	}

	defaultIPSANs = []string{
		"127.0.0.1",
	}
)

func getEnvOrDefault(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func discoverHostIPs() []net.IP {
	var ips []net.IP
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		return ips
	}
	for _, addr := range addrs {
		if ipNet, ok := addr.(*net.IPNet); ok && !ipNet.IP.IsLoopback() {
			if ip4 := ipNet.IP.To4(); ip4 != nil {
				ips = append(ips, ip4)
			}
		}
	}
	return ips
}

func main() {
	outDir := flag.String("out", getEnvOrDefault("CERT_OUT_DIR", filepath.Join("deploy", "mosquitto", "certs")), "Output directory for certificates")
	force := flag.Bool("force", getEnvOrDefault("CERT_FORCE", "false") == "true", "Overwrite existing certificate files if they already exist")
	country := flag.String("country", getEnvOrDefault("CERT_COUNTRY", "US"), "Country code for certificate subjects")
	caOrg := flag.String("ca-org", getEnvOrDefault("CERT_CA_ORG", "BombeCam Security Foundation"), "CA certificate organization")
	caCN := flag.String("ca-cn", getEnvOrDefault("CERT_CA_CN", "BombeCam Local Root CA"), "CA certificate common name")
	serverCN := flag.String("server-cn", getEnvOrDefault("CERT_SERVER_CN", "mqtts02-us.osaio.net"), "Server certificate common name")
	dnsSANsFlag := flag.String("dns-sans", getEnvOrDefault("CERT_DNS_SANS", strings.Join(defaultDNSSANs, ",")), "Comma-separated DNS SANs")
	ipSANsFlag := flag.String("ip-sans", getEnvOrDefault("CERT_IP_SANS", strings.Join(defaultIPSANs, ",")), "Comma-separated IP SANs")
	autoIPs := flag.Bool("auto-ips", getEnvOrDefault("CERT_AUTO_IPS", "true") == "true", "Automatically discover host IPv4 addresses for SANs")

	flag.Usage = func() {
		fmt.Fprintf(flag.CommandLine.Output(), "BombeCam Certificate Generator (bombecam-certgen)\n\nUsage:\n  bombecam-certgen [flags]\n\nFlags:\n")
		flag.PrintDefaults()
	}

	flag.Parse()

	if err := os.MkdirAll(*outDir, 0755); err != nil {
		fmt.Fprintf(os.Stderr, "Failed to create directory %s: %v\n", *outDir, err)
		os.Exit(1)
	}

	certFiles := []string{"ca.crt", "ca.key", "server.crt", "server.key"}
	if !*force {
		for _, name := range certFiles {
			p := filepath.Join(*outDir, name)
			if _, err := os.Stat(p); err == nil {
				fmt.Fprintf(os.Stderr, "Refusing to overwrite existing certificate material: %s (use -force to overwrite)\n", p)
				os.Exit(1)
			} else if !os.IsNotExist(err) {
				fmt.Fprintf(os.Stderr, "Error checking file %s: %v\n", p, err)
				os.Exit(1)
			}
		}
	}

	// Parse DNS SANs
	var dnsSANs []string
	for _, s := range strings.Split(*dnsSANsFlag, ",") {
		s = strings.TrimSpace(s)
		if s != "" {
			dnsSANs = append(dnsSANs, s)
		}
	}

	// Parse IP SANs
	var ipSANs []net.IP
	seenIP := make(map[string]bool)
	for _, s := range strings.Split(*ipSANsFlag, ",") {
		s = strings.TrimSpace(s)
		if s != "" {
			ip := net.ParseIP(s)
			if ip == nil {
				fmt.Fprintf(os.Stderr, "Invalid IP SAN %q\n", s)
				os.Exit(1)
			}
			if !seenIP[ip.String()] {
				seenIP[ip.String()] = true
				ipSANs = append(ipSANs, ip)
			}
		}
	}

	// Auto-discover host IPs if enabled
	if *autoIPs {
		discovered := discoverHostIPs()
		for _, ip := range discovered {
			if !seenIP[ip.String()] {
				seenIP[ip.String()] = true
				ipSANs = append(ipSANs, ip)
			}
		}
	}

	fmt.Printf("Generating MQTTS certificates in: %s (SAN IPs: %v)\n", *outDir, ipSANs)

	// 1. Generate CA Key & Cert
	caKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Failed to generate CA private key: %v\n", err)
		os.Exit(1)
	}

	caPubKeyDER := x509.MarshalPKCS1PublicKey(&caKey.PublicKey)
	caSKID := sha1.Sum(caPubKeyDER)

	caTemplate := &x509.Certificate{
		SerialNumber: big.NewInt(1001),
		Subject: pkix.Name{
			Organization: []string{*caOrg},
			CommonName:   *caCN,
			Country:      []string{*country},
		},
		NotBefore:             time.Now().Add(-24 * time.Hour),
		NotAfter:              time.Now().Add(10 * 365 * 24 * time.Hour), // 10 years
		IsCA:                  true,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign | x509.KeyUsageDigitalSignature,
		BasicConstraintsValid: true,
		SubjectKeyId:          caSKID[:],
	}

	caCertDER, err := x509.CreateCertificate(rand.Reader, caTemplate, caTemplate, &caKey.PublicKey, caKey)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Failed to create CA certificate: %v\n", err)
		os.Exit(1)
	}

	caCert, err := x509.ParseCertificate(caCertDER)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Failed to parse generated CA certificate: %v\n", err)
		os.Exit(1)
	}

	if err := writePEM(filepath.Join(*outDir, "ca.crt"), "CERTIFICATE", caCertDER, *force); err != nil {
		fmt.Fprintf(os.Stderr, "Failed to write ca.crt: %v\n", err)
		os.Exit(1)
	}
	if err := writePEM(filepath.Join(*outDir, "ca.key"), "RSA PRIVATE KEY", x509.MarshalPKCS1PrivateKey(caKey), *force); err != nil {
		fmt.Fprintf(os.Stderr, "Failed to write ca.key: %v\n", err)
		os.Exit(1)
	}
	fmt.Println("[+] CA certificate and key written.")

	// 2. Generate Server Key & Cert
	serverKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Failed to generate Server private key: %v\n", err)
		os.Exit(1)
	}

	serverPubKeyDER := x509.MarshalPKCS1PublicKey(&serverKey.PublicKey)
	serverSKID := sha1.Sum(serverPubKeyDER)

	serverTemplate := &x509.Certificate{
		SerialNumber: big.NewInt(1002),
		Subject: pkix.Name{
			Organization: []string{*caOrg},
			CommonName:   *serverCN,
			Country:      []string{*country},
		},
		NotBefore:      time.Now().Add(-24 * time.Hour),
		NotAfter:       time.Now().Add(5 * 365 * 24 * time.Hour), // 5 years
		KeyUsage:       x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:    []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth},
		DNSNames:       dnsSANs,
		IPAddresses:    ipSANs,
		SubjectKeyId:   serverSKID[:],
		AuthorityKeyId: caCert.SubjectKeyId,
	}

	serverCertDER, err := x509.CreateCertificate(rand.Reader, serverTemplate, caCert, &serverKey.PublicKey, caKey)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Failed to create Server certificate: %v\n", err)
		os.Exit(1)
	}

	if err := writePEM(filepath.Join(*outDir, "server.crt"), "CERTIFICATE", serverCertDER, *force); err != nil {
		fmt.Fprintf(os.Stderr, "Failed to write server.crt: %v\n", err)
		os.Exit(1)
	}
	if err := writePEM(filepath.Join(*outDir, "server.key"), "RSA PRIVATE KEY", x509.MarshalPKCS1PrivateKey(serverKey), *force); err != nil {
		fmt.Fprintf(os.Stderr, "Failed to write server.key: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("[+] Server certificate written with SANs:\n    DNS: %v\n    IP: %v\n", dnsSANs, ipSANs)
}

func writePEM(path, blockType string, data []byte, force bool) error {
	flags := os.O_WRONLY | os.O_CREATE
	if force {
		flags |= os.O_TRUNC
	} else {
		flags |= os.O_EXCL
	}
	f, err := os.OpenFile(path, flags, 0600)
	if err != nil {
		return err
	}
	defer f.Close()

	return pem.Encode(f, &pem.Block{Type: blockType, Bytes: data})
}
