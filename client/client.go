package main

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	log "github.com/golang/glog"

	"github.com/openconfig/bootz/proto/bootz"
	"go.mozilla.org/pkcs7"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/protobuf/proto"
)

// Represents a 128 bit nonce.
const nonceLength = 16

var (
	verifyTLSCert = flag.Bool("verify_tls_cert", false, "Whether to verify the TLS certificate presented by the Bootz server. If false, all TLS connections are implicity trusted.")
	insecureBoot  = flag.Bool("insecure_boot", false, "Whether to start the emulated device in non-secure mode. This informs Bootz server to not provide ownership certificates or vouchers.")
	bootzAddress  = flag.String("address", "8008", "The [ip:]port to listen for the bootz server to connect. when ip is not given, the server will connect to localhost.")
	rootCA        = flag.String("root_ca_cert_path", "../testdata/vendorca_pub.pem", "The relative path to a file containing a PEM encoded certificate for the manufacturer CA.")
)

type OwnershipVoucher struct {
	OV OwnershipVoucherInner `json:"ietf-voucher:voucher"`
}

// Defines the Ownership Voucher format. See https://www.rfc-editor.org/rfc/rfc8366.html.
type OwnershipVoucherInner struct {
	CreatedOn                  string `json:"created-on"`
	ExpiresOn                  string `json:"expires-on"`
	SerialNumber               string `json:"serial-number"`
	Assertion                  string `json:"assertion"`
	PinnedDomainCert           string `json:"pinned-domain-cert"`
	DomainCertRevocationChecks bool   `json:"domain-cert-revocation-checks"`
}

// pemEncodeCert adds the correct PEM headers and footers to a raw certificate block.
func pemEncodeCert(contents string) string {
	return strings.Join([]string{"-----BEGIN CERTIFICATE-----", contents, "-----END CERTIFICATE-----"}, "\n")
}

// convert address to localhost when no ip is specefied
func convertAddress(addr string) string {
	items := strings.Split(addr, ":")
	listenAddr := addr
	if len(items) == 1 {
		listenAddr = fmt.Sprintf("localhost:%v", addr)
	}
	return listenAddr
}

// validateArtifacts checks the signed artifacts in a GetBootstrapDataResponse. Specifically, it:
// - Checks that the OV in the response is signed by the manufacturer.
// - Checks that the serial number in the OV matches the one in the original request.
// - Verifies that the Ownership Certificate is in the chain of signers of the Pinned Domain Cert.
func validateArtifacts(serialNumber string, resp *bootz.GetBootstrapDataResponse, rootCA []byte) error {
	ov64 := resp.GetOwnershipVoucher()
	if len(ov64) == 0 {
		return fmt.Errorf("received empty ownership voucher from server")
	}

	oc := resp.GetOwnershipCertificate()
	if len(oc) == 0 {
		return fmt.Errorf("received empty ownership certificate from server")
	}

	// Decode the ownership voucher
	log.Infof("Decoding ownership voucher...")
	ov, err := base64.StdEncoding.DecodeString(string(ov64))
	if err != nil {
		return err
	}

	// Parse the PKCS7 message
	log.Infof("Parsing PKCS7 message in OV...")
	p7, err := pkcs7.Parse(ov)
	if err != nil {
		return err
	}

	// Unmarshal the ownership voucher into a struct.
	log.Infof("Unmarshalling OV into a struct...")
	parsedOV := OwnershipVoucher{}
	err = json.Unmarshal(p7.Content, &parsedOV)
	if err != nil {
		return err
	}

	// Create a CA pool for the device to validate that the vendor has signed this OV.
	log.Infof("Creating a CA pool for the device to validate the vendor has signed this OV")
	vendorCAPool := x509.NewCertPool()
	if !vendorCAPool.AppendCertsFromPEM(rootCA) {
		return fmt.Errorf("unable to add vendor root CA to pool")
	}
	log.Infof("=============================================================================")

	// Verify the ownership voucher with this CA.
	log.Infof("Verifying the OV with this CA")
	err = p7.VerifyWithChain(vendorCAPool)
	if err != nil {
		return err
	}
	log.Infof("Validated ownership voucher signed by vendor")

	log.Infof("=============================================================================")

	// Verify the serial number for this OV
	log.Infof("Verifying the serial number for this OV")
	if parsedOV.OV.SerialNumber != serialNumber {
		return fmt.Errorf("serial number from OV does not match request")
	}
	log.Infof("Verified serial number is %v", serialNumber)

	log.Infof("Adding PEM headers and footers to OV")
	pdCPEM := pemEncodeCert(parsedOV.OV.PinnedDomainCert)

	// Create a new pool with this PDC.
	log.Infof("Creating a new pool with the PDC")
	pdcPool := x509.NewCertPool()
	if !pdcPool.AppendCertsFromPEM([]byte(pdCPEM)) {
		return err
	}

	// Parse the Ownership Certificate.
	log.Infof("Parsing the OC")
	ocCert, err := certFromPemBlock(oc)
	if err != nil {
		return fmt.Errorf("failed to parse certificate: %v", err)
	}

	// Verify that the OC is signed by the PDC.
	log.Infof("Verifying that the OC is signed by the PDC")
	opts := x509.VerifyOptions{
		Roots:         pdcPool,
		Intermediates: x509.NewCertPool(),
	}
	if _, err := ocCert.Verify(opts); err != nil {
		return err
	}
	log.Infof("Validated ownership certificate with OV PDC")

	// Validate the response signature.
	log.Infof("=============================================================================")
	log.Infof("===================== Validating the response signature =====================")
	log.Infof("=============================================================================")
	log.Infof("Marshalling the response...")
	signedResponseBytes, err := proto.Marshal(resp.GetSignedResponse())
	if err != nil {
		return err
	}
	log.Infof("Sucessfully serialized the response")

	log.Infof("Calculating the sha256 sum to validate the response signature...")
	hashed := sha256.Sum256(signedResponseBytes)
	log.Infof("Decoding the response...")
	decodedSig, err := base64.StdEncoding.DecodeString(resp.GetResponseSignature())
	if err != nil {
		return err
	}
	log.Infof("Decoded the response string")

	log.Infof("Using the ownership certificate's public key to verify the signature... Note only RSA keys are supported")
	// Verify the signature with the ownership certificate's public key. Currently only RSA keys are supported.
	switch pub := ocCert.PublicKey.(type) {
	case *rsa.PublicKey:
		err = rsa.VerifyPKCS1v15(pub, crypto.SHA256, hashed[:], decodedSig)
		if err != nil {
			return fmt.Errorf("signature not verified: %v", err)
		}
	default:
		return fmt.Errorf("unsupported public key type: %T", pub)
	}
	log.Infof("Verified SignedResponse signature")
	return nil
}

func certFromPemBlock(data []byte) (*x509.Certificate, error) {
	block, _ := pem.Decode(data)
	if block == nil {
		return nil, fmt.Errorf("failed to parse certificate PEM")
	}
	return x509.ParseCertificate(block.Bytes)
}

// generateNonce() generates a fixed-length nonce.
func generateNonce() (string, error) {
	b := make([]byte, nonceLength)
	_, err := rand.Read(b)
	if err != nil {
		return "", err
	}
	return base64.StdEncoding.EncodeToString(b), nil
}

func main() {
	ctx := context.Background()
	flag.Parse()
	log.Infof("=============================================================================")
	log.Infof("=========================== BootZ Client Emulator ===========================")
	log.Infof("=============================================================================")

	log.Infof("=============================================================================")
	log.Infof("======================== Loading Root CA Certificate ========================")
	log.Infof("=============================================================================")
	if *rootCA == "" {
		log.Exitf("No Root CA certificate file specified")
	}
	log.Infof("Reading Root CA certificate file...")
	rootCABytes, err := os.ReadFile(*rootCA)
	if err != nil {
		log.Exitf("Error opening Root CA file: %v", err)
	}
	log.Infof("Successfully read Root CA certificate file")

	// Verify the Root CA cert is valid.
	log.Infof("Verifying Root CA certificate...")
	caCert, err := certFromPemBlock(rootCABytes)
	if err != nil {
		log.Exitf("Error parsing Root CA certificate")
	}
	log.Infof("Loaded Root CA certificate: %v", string(caCert.Subject.CommonName))

	log.Infof("=============================================================================")
	log.Infof("================== Constructing a fake device for testing ===================")
	log.Infof("=============================================================================")
	// Construct the fake device.
	// TODO: Allow these values to be set e.g. via a flag.
	chassis := bootz.ChassisDescriptor{
		Manufacturer: "Cisco",
		SerialNumber: "123",
		ControlCards: []*bootz.ControlCard{
			{
				SerialNumber: "123A",
				Slot:         1,
				PartNumber:   "123A",
			},
			{
				SerialNumber: "123B",
				Slot:         2,
				PartNumber:   "123B",
			},
		},
	}

	log.Infof("%v chassis %v starting with SecureOnly = %v", chassis.Manufacturer, chassis.SerialNumber, !*insecureBoot)

	// 1. DHCP Discovery of Bootstrap Server
	// This step emulates the retrieval of the bootz server IP
	// address from a DHCP server. In this case we always connect to localhost.
	log.Infof("=============================================================================")
	log.Infof("================ Starting DHCP discovery of bootstrap server ================")
	log.Infof("=============================================================================")
	if *bootzAddress == "" {
		log.Exitf("No port provided.")
	}
	bootzAddress := fmt.Sprintf("localhost:%v", convertAddress(*bootzAddress))
	log.Infof("Connecting to bootz server at address %q", bootzAddress)

	// 2. Bootstrapping Service
	// Device initiates a TLS-secured gRPC connection with the Bootz server.
	tlsConfig := &tls.Config{InsecureSkipVerify: !*verifyTLSCert}
	conn, err := grpc.Dial(bootzAddress, grpc.WithTransportCredentials(credentials.NewTLS(tlsConfig)))
	if err != nil {
		log.Exitf("Client unable to connect to Bootstrap Server: %v", err)
	}
	defer conn.Close()
	log.Infof("Creating a new bootstrap client")
	c := bootz.NewBootstrapClient(conn)
	log.Infof("Client connected to bootz server")

	// This is the active control card making the bootz request.
	log.Infof("=============================================================================")
	log.Infof("Setting active control card with serial number: %v, slot: %v, part number: %v",
		chassis.ControlCards[0].SerialNumber, chassis.ControlCards[0].Slot, chassis.ControlCards[0].PartNumber)
	activeControlCard := chassis.ControlCards[0]

	nonce := ""
	if !*insecureBoot {
		log.Infof("Device in secure boot mode, generating a nonce that the Bootz server will use to sign the response")
		// Generate a nonce that the Bootz server will use to sign the response.
		nonce, err = generateNonce()
		if err != nil {
			log.Exitf("Error generating nonce: %v", err)
		}
		log.Infof("Nonce of %v generated successfully", nonce)
	}

	log.Infof("=============================================================================")
	log.Infof("======================== Retrieving bootstrap data ==========================")
	log.Infof("=============================================================================")
	log.Infof("Building bootstrap data request")
	req := &bootz.GetBootstrapDataRequest{
		ChassisDescriptor: &chassis,
		// This is the active control card, e.g. the one making the bootz request.
		ControlCardState: &bootz.ControlCardState{
			SerialNumber: activeControlCard.GetSerialNumber(),
			Status:       bootz.ControlCardState_CONTROL_CARD_STATUS_NOT_INITIALIZED,
		},
		Nonce: nonce,
	}
	log.Infof("Built bootstrap data request with %v chassis %v and control card %v with status %v and nonce %v",
		req.ChassisDescriptor.Manufacturer, req.ChassisDescriptor.SerialNumber, req.ControlCardState.SerialNumber, req.ControlCardState.Status, req.Nonce)

	// Get bootstrapping data from Bootz server
	// TODO: Extract and parse response.
	log.Infof("Requesting Bootstrap Data from Bootz server")
	resp, err := c.GetBootstrapData(ctx, req)
	if err != nil {
		log.Exitf("Error calling GetBootstrapData: %v", err)
	}
	log.Infof("Successfully retrieved Bootstrap Data from server")

	// Only check OC, OV and response signature if SecureOnly is set.
	if !*insecureBoot {
		log.Infof("=============================================================================")
		log.Infof("====================== Validating response signature ========================")
		log.Infof("=============================================================================")
		if err := validateArtifacts(activeControlCard.GetSerialNumber(), resp, rootCABytes); err != nil {
			log.Exitf("Error validating signed data: %v", err)
		}
	}

	signedResp := resp.GetSignedResponse()
	if !*insecureBoot && signedResp.GetNonce() != nonce {
		log.Exitf("GetBootstrapDataResponse nonce does not match")
	}

	// TODO: Verify the hash of the intended image.
	// Simply print out the received configs we get. This section should actually contain the logic to verify and install the images and config.
	log.Infof("=============================================================================")
	log.Infof("===================== Processing control card configs =======================")
	log.Infof("=============================================================================")
	for _, data := range signedResp.GetResponses() {
		log.Infof("Received config for control card %v", data.GetSerialNum())
		log.Infof("Downloading image %+v...", data.GetIntendedImage())
		time.Sleep(time.Second * 5)
		log.Infof("Done")
		log.Infof("Installing boot config %+v...", data.GetBootConfig())
		time.Sleep(time.Second * 5)
		log.Infof("Done")
		log.Infof("=============================================================================")
	}

	// 6. ReportProgress
	log.Infof("=========================== Sending Status Report ===========================")
	log.Infof("=============================================================================")
	statusReq := &bootz.ReportStatusRequest{
		Status:        bootz.ReportStatusRequest_BOOTSTRAP_STATUS_SUCCESS,
		StatusMessage: "Bootstrap Success",
		States: []*bootz.ControlCardState{
			{
				Status:       bootz.ControlCardState_CONTROL_CARD_STATUS_INITIALIZED,
				SerialNumber: chassis.GetControlCards()[0].GetSerialNumber(),
			},
			{
				Status:       bootz.ControlCardState_CONTROL_CARD_STATUS_INITIALIZED,
				SerialNumber: chassis.GetControlCards()[1].GetSerialNumber(),
			},
		},
	}

	_, err = c.ReportStatus(ctx, statusReq)
	if err != nil {
		log.Exitf("Error reporting status: %v", err)
	}
	log.Infof("Status report sent")
	// At this point the device has minimal configuration and can receive further gRPC calls. After this, the TPM Enrollment and attestation occurs.
}
