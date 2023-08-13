// Bootz server reference implementation.
//
// The bootz server will provide a simple file based bootstrap
// implementation for devices.  The service can be extended by
// provding your own implementation of the entity manager.
package main

import (
	"flag"
	"net"
	"fmt"

	//"github.com/labstack/gommon/log"
	"github.com/openconfig/bootz/proto/bootz"
	//"github.com/openconfig/bootz/server/entitymanager"
	"github.com/openconfig/bootz/server/service"
	"github.com/openconfig/bootz/server/entitymanager"
	"google.golang.org/grpc"

	log "github.com/golang/glog"
)

var (
	port = flag.String("port", "", "The port to start the Bootz server on localhost")
)

func main() {
	flag.Parse()

	if *port == "" {
		log.Exitf("no port selected. specify with the -port flag")
	}
	em, err := entitymanager.New("test")
	c := service.New(em)
	s := grpc.NewServer()

	lis, err := net.Listen("tcp", fmt.Sprintf("localhost:%v", *port))
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
