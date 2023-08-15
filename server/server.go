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
	"net/http"
	"os"
	"strings"

	"google.golang.org/grpc/credentials"

	"github.com/openconfig/bootz/dhcp"
	"github.com/openconfig/bootz/proto/bootz"
	"github.com/openconfig/bootz/server/entitymanager"
	"github.com/openconfig/bootz/server/service"
	"google.golang.org/grpc"

	log "github.com/golang/glog"
)

var (
	insecureBoot       = flag.Bool("insecure_boot", false, "Whether to start the emulated device in non-secure mode. This informs Bootz server to not provide ownership certificates or vouchers.")
	bootzAddress       = flag.String("address", "", "The [ip:]port to listen for the bootz server. when ip is not given, the server will listen on local host. ip should be specific (other than local host) when the client does not run on the local host.")
	rootCA             = flag.String("root_ca_cert_path", "../testdata/ca.pem", "The relative path to a file contained a PEM encoded certificate for the manufacturer CA.")
	cert               = flag.String("server_cert_path", "../testdata/servercert.pem", "The relative path to a file contained a PEM encoded certificate for the bootz server, that can be verified using root ca.")
	key                = flag.String("server_key_path", "../testdata/serverkey.pem", "The relative path to a file contained a PEM encoded private key for the bootz server, that can be verified using root ca.")
	dhcpServerAddress  = flag.String("dhcp_address", "", "The ip to listen for the dhcp server. when ip is not given, the dhcp server will not start. root access is required for dhcp.")
	imageServerAddress = flag.String("image_server_address", "3000", "The [ip:]port to listen for the image server. when ip is not given, the server will listen on local host. ip should be specific (other than local host) when the client does not run on the local host.")
	imagesLocation     = flag.String("image_location", "/tmp/bootz/images", "The directory where the images will reside. The defaults is /tmp/bootz/images")
)

// load trust bundle and client key and certificate
func loadCertificates(rootCaFile, certFile, keyFile string) (*x509.CertPool, tls.Certificate, error) {
	if rootCaFile == "" || keyFile == "" || certFile == "" {
		return nil, tls.Certificate{}, fmt.Errorf("ca file, cert file, and key file need to be set")
	}
	caCertBytes, err := os.ReadFile(rootCaFile)
	if err != nil {
		return nil, tls.Certificate{}, err
	}
	trusBundle := x509.NewCertPool()
	if !trusBundle.AppendCertsFromPEM(caCertBytes) {
		return nil, tls.Certificate{}, err
	}
	keyPair, err := tls.LoadX509KeyPair(certFile, keyFile)
	if err != nil {
		return nil, tls.Certificate{}, err
	}
	return trusBundle, keyPair, nil

}

func convertAddress(addr string) string {
	items := strings.Split(addr, ":")
	listenAddr := addr
	if len(items) == 1 {
		listenAddr = fmt.Sprintf("localhost:%v", addr)
	}
	return listenAddr
}

func main() {
	flag.Parse()

	em, err := entitymanager.New("test")
	if err != nil {
		log.Exitf("Could not initialize an entity manage %v", err)
	}
	c := service.New(em)

	// load ca certificate
	if *rootCA == "" || *cert == "" || *key == "" {
		log.Exitf("No root CA certificate (root_ca_cert_path), or server certificate (server_cert_path), or server private key (server_key_path) not specified")
	}
	ca, serverCert, err := loadCertificates(*rootCA, *cert, *key)
	if err != nil {
		log.Exitf("Could not load certificates and root ca: %v", err)
	}
	opts := []grpc.ServerOption{}
	tls := &tls.Config{
		Certificates: []tls.Certificate{serverCert},
		RootCAs:      ca,
	}

	dhcpSrv, err := dhcp.New(em)
	if err != nil {
		log.Exitf("Failed to create dhcp server: %v", err)
	}

	err = dhcpSrv.Start()
	if err != nil {
		log.Exitf("Count not start dhcp server: %v", err)
	}
	defer dhcpSrv.Stop()

	go func() {
		// code to start image server
		// exit the code if the address is given, but server can not be started
		// use the same certificate loaded above for image server
		fs := http.FileServer(http.Dir(*imagesLocation))
		http.Handle("/", fs)
		if err := http.ListenAndServeTLS(convertAddress(*imageServerAddress), *cert, *key, fs); err != nil {
			log.Fatalf("Error starting image server: %v", err)
		}
	}()

	tlsConfig := credentials.NewTLS(tls)
	opts = append(opts, grpc.Creds(tlsConfig))
	s := grpc.NewServer(opts...)
	lis, err := net.Listen("tcp", convertAddress(*bootzAddress))
	if err != nil {
		log.Exitf("Error listening on port: %v", err)
	}
	log.Infof("Listening on %s", lis.Addr())
	bootz.RegisterBootstrapServer(s, c)
	err = s.Serve(lis)
	if err != nil {
		log.Exitf("Error serving grpc: %v", err)
	}
}
