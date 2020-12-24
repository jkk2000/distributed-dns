package main

import (
	"flag"
	"fmt"
	"log"
	"math/rand"
	"net"
	"net/http"
	"os"
	"testing"
	"time"

	"github.com/gorilla/mux"
	"github.com/krithikvaidya/distributed-dns/replicated_kv_store/protos"
	"google.golang.org/grpc"
)

var n_replica int

func init() {

	/*
	 * Workaround for a Go bug
	 * The Init() function for the testing package should be called
	 * before our init() function for parsing the command-line arguments
	 * of the `go test` command
	 */
	testing.Init()

	// Command line parameters
	flag.IntVar(&n_replica, "n", 5, "total number of replicas (default=5)")
	flag.Parse()

	log.SetFlags(0) // Turn off timestamps in log output.
	rand.Seed(time.Now().UnixNano())

}

func start_key_value_replica(addr string, done chan bool) {
	kv := newStore()
	r := mux.NewRouter()
	r.HandleFunc("/kvstore", kv.kvstoreHandler).Methods("GET")
	r.HandleFunc("/{key}", kv.postHandler).Methods("POST")
	r.HandleFunc("/{key}", kv.getHandler).Methods("GET")
	r.HandleFunc("/{key}", kv.putHandler).Methods("PUT")
	r.HandleFunc("/{key}", kv.deleteHandler).Methods("DELETE")

	//Start the server and listen for requests
	fmt.Printf("Starting server at port %s\n", addr)
	done <- true
	if err := http.ListenAndServe(addr, r); err != nil {
		log.Fatal(err)
	}
}

func main() {

	fmt.Println("\nRaft-based Replicated Key Value Store")

	fmt.Printf("Enter the replica's id: ")
	var rid int32
	fmt.Scanf("%d", &rid)

	fmt.Printf("\nEnter the TCP network address that the replica should bind to (eg - :7890): ")
	var address string
	fmt.Scanf("%s", &address)

	tcpAddr, err := net.ResolveTCPAddr("tcp4", address)
	CheckError(err)

	listener, err := net.ListenTCP("tcp", tcpAddr)
	CheckError(err)

	fmt.Printf("\nSuccessfully bound to address %v\n", address)
	var addresskeyvalue string
	fmt.Printf("\nEnter port to run key-value replica: ")
	fmt.Scanf("%s", &addresskeyvalue)

	done := make(chan bool, 1)
	go start_key_value_replica(addresskeyvalue, done)
	<-done

	fmt.Printf("\nEnter the addresses of %v other replicas: \n", n_replica-1)

	rep_addrs := make([]string, n_replica)

	for i := int32(0); i < int32(n_replica); i++ {

		if i == rid {
			continue
		}

		fmt.Scanf("%s", &rep_addrs[i])

	}

	grpcServer := grpc.NewServer()

	// InitializeNode() is defined in raft_node.go
	node := InitializeNode(int32(n_replica), rid, addresskeyvalue)

	// ConsensusService is defined in protos/replica.proto./
	// RegisterConsensusServiceServer is present in the generated .pb.go file
	protos.RegisterConsensusServiceServer(grpcServer, node)

	// gRPC Serve is blocking, so we do it on a separate goroutine
	go func() {

		err := grpcServer.Serve(listener)

		if err != nil {
			log.Printf("\nError in gRPC Serve: %v\n", err)
			os.Exit(1)
		}

	}()

	fmt.Printf("\ngRPC server listening...\n")

	fmt.Printf("\nPress enter when all other nodes are online.\n")
	var input rune
	fmt.Scanf("%c", &input)

	// Attempt to gRPC dial to other replicas. ConnectToPeerReplicas is defined in raft_node.go
	fmt.Printf("\nAttempting to connect to peer replicas...\n")
	node.ConnectToPeerReplicas(rep_addrs)
	log.Printf("\nSuccessfully connected to peer replicas.\n")
	<-node.ready_chan // wait until all connections to our have been established.
	log.Printf("\nAll peer replicas have successfully connected.\n")
	// this goroutine will keep monitoring all connections and try to re-establish connections that die
	// go node.MonitorConnections()

	// dummy channel to ensure program doesn't exit. Remove it later
	all_connected := make(chan bool)
	<-all_connected

}
