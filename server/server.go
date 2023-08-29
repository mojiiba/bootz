// Bootz server reference implementation.
//
// The bootz server will provide a simple file based bootstrap
// implementation for devices.  The service can be extended by
// provding your own implementation of the entity manager.
package main

import (
	"crypto/tls"
	"crypto/x509"
	"flag"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"

	"github.com/openconfig/bootz/proto/bootz"
	"github.com/openconfig/bootz/server/entitymanager"
	"github.com/openconfig/bootz/server/service"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"

	log "github.com/golang/glog"
)

var (
	bootzAddress      = flag.String("address", "8008", "The [ip:]port to listen for the bootz request. when ip is not given, the server will listen on localhost.")
	artifactDirectory = flag.String("artifact_dir", "../testdata/", "The relative directory to look into for certificates, private keys and OVs.")
)

// convert address to localhost when no ip is specefied
func convertAddress(addr string) string {
	items := strings.Split(addr, ":")
	listenAddr := addr
	if len(items) == 1 {
		listenAddr = fmt.Sprintf("localhost:%v", addr)
	}
	return listenAddr
}

// readKeyPair reads the cert/key pair from the specified artifacts directory.
// Certs must have the format {name}_pub.pem and keys must have the format {name}_priv.pem
func readKeypair(name string) (*service.KeyPair, error) {
	cert, err := os.ReadFile(filepath.Join(*artifactDirectory, fmt.Sprintf("%v_pub.pem", name)))
	if err != nil {
		return nil, fmt.Errorf("unable to read %v cert: %v", name, err)
	}
	key, err := os.ReadFile(filepath.Join(*artifactDirectory, fmt.Sprintf("%v_priv.pem", name)))
	if err != nil {
		return nil, fmt.Errorf("unable to read %v key: %v", name, err)
	}
	return &service.KeyPair{
		Cert: string(cert),
		Key:  string(key),
	}, nil
}

// readOVs discovers and reads all available OVs in the artifacts directory.
func readOVs() (service.OVList, error) {
	ovs := make(service.OVList)
	files, err := os.ReadDir(*artifactDirectory)
	if err != nil {
		return nil, fmt.Errorf("unable to list files in artifact directory: %v", err)
	}
	for _, f := range files {
		if strings.HasPrefix(f.Name(), "ov") {
			bytes, err := os.ReadFile(filepath.Join(*artifactDirectory, f.Name()))
			if err != nil {
				return nil, err
			}
			trimmed := strings.TrimPrefix(f.Name(), "ov_")
			trimmed = strings.TrimSuffix(trimmed, ".txt")
			ovs[trimmed] = string(bytes)
		}
	}
	if len(ovs) == 0 {
		return nil, fmt.Errorf("found no OVs in artifacts directory")
	}
	return ovs, err
}

// generateServerTlsCert creates a new TLS keypair from the PDC.
func generateServerTlsCert(pdc *service.KeyPair) (*tls.Certificate, error) {
	tlsCert, err := tls.X509KeyPair([]byte(pdc.Cert), []byte(pdc.Key))
	if err != nil {
		return nil, fmt.Errorf("unable to generate Server TLS Certificate from PDC %v", err)
	}
	return &tlsCert, err
}

// parseSecurityArtifacts reads from the specified directory to find the required keypairs and ownership vouchers.
func parseSecurityArtifacts() (*service.SecurityArtifacts, error) {
	oc, err := readKeypair("oc")
	if err != nil {
		return nil, err
	}
	pdc, err := readKeypair("pdc")
	if err != nil {
		return nil, err
	}
	vendorCA, err := readKeypair("vendorca")
	if err != nil {
		return nil, err
	}
	ovs, err := readOVs()
	if err != nil {
		return nil, err
	}
	tlsCert, err := generateServerTlsCert(pdc)
	if err != nil {
		return nil, err
	}
	return &service.SecurityArtifacts{
		OC:         oc,
		PDC:        pdc,
		VendorCA:   vendorCA,
		OV:         ovs,
		TLSKeypair: tlsCert,
	}, nil
}

func main() {
	flag.Parse()

	log.Infof("=============================================================================")
	log.Infof("=========================== BootZ Server Emulator ===========================")
	log.Infof("=============================================================================")

	if *bootzAddress == "" {
		log.Exitf("no port selected. specify with the -port flag")
	}
	if *artifactDirectory == "" {
		log.Exitf("no artifact directory specified")
	}

	log.Infof("Setting up server security artifacts: OC, OVs, PDC, VendorCA")
	sa, err := parseSecurityArtifacts()
	if err != nil {
		log.Exit(err)
	}

	log.Infof("Setting up entities")
	em := entitymanager.New(sa)
	em.AddChassis(bootz.BootMode_BOOT_MODE_SECURE, "Cisco", "123").AddControlCard("123A").AddControlCard("123B")
	c := service.New(em)

	trustBundle := x509.NewCertPool()
	if !trustBundle.AppendCertsFromPEM([]byte(sa.PDC.Cert)) {
		log.Exitf("unable to add PDC cert to trust pool")
	}
	tls := &tls.Config{
		Certificates: []tls.Certificate{*sa.TLSKeypair},
		RootCAs:      trustBundle,
	}
	log.Infof("Creating server...")
	s := grpc.NewServer(grpc.Creds(credentials.NewTLS(tls)))

	lis, err := net.Listen("tcp", convertAddress(*bootzAddress))
	if err != nil {
		log.Exitf("Error listening on port: %v", err)
	}
	log.Infof("Server ready and listening on %s", lis.Addr())
	log.Infof("=============================================================================")
	bootz.RegisterBootstrapServer(s, c)
	err = s.Serve(lis)
	if err != nil {
		log.Exitf("Error serving grpc: %v", err)
	}
}
