package main

import (
	"bufio"
	"encoding/binary"
	"flag"
	"fmt"
	"log"
	"math/rand"
	"net"
	"os"
	"sync"
	"time"
)

// Message represents the structured UDP message
type Message struct {
	SequenceNumber uint32
	Payload        []byte
	Timestamp      int64
}

// encodeMessage converts a Message to a byte slice
func encodeMessage(msg Message) []byte {
	data := make([]byte, 4+len(msg.Payload)+8)
	binary.BigEndian.PutUint32(data[:4], msg.SequenceNumber)
	copy(data[4:4+len(msg.Payload)], msg.Payload)
	binary.BigEndian.PutUint64(data[4+len(msg.Payload):], uint64(msg.Timestamp))
	return data
}

// decodeMessage converts a byte slice back to a Message
func decodeMessage(data []byte) Message {
	seqNum := binary.BigEndian.Uint32(data[:4])
	payload := data[4 : len(data)-8]
	timestamp := int64(binary.BigEndian.Uint64(data[len(data)-8:]))

	return Message{
		SequenceNumber: seqNum,
		Payload:        payload,
		Timestamp:      timestamp,
	}
}

// Client implementation
type Client struct {
	serverAddr     *net.UDPAddr
	conn           *net.UDPConn
	sequenceNumber uint32
	mu             sync.Mutex
}

func NewClient(serverHost string, serverPort int) (*Client, error) {
	serverAddr, err := net.ResolveUDPAddr("udp", fmt.Sprintf("%s:%d", serverHost, serverPort))
	if err != nil {
		return nil, err
	}

	conn, err := net.DialUDP("udp", nil, serverAddr)
	if err != nil {
		return nil, err
	}

	return &Client{
		serverAddr:     serverAddr,
		conn:           conn,
		sequenceNumber: 0,
	}, nil
}

func (c *Client) SendMessage(payload []byte) error {
	c.mu.Lock()
	seqNum := c.sequenceNumber
	c.sequenceNumber++
	c.mu.Unlock()

	msg := Message{
		SequenceNumber: seqNum,
		Payload:        payload,
		Timestamp:      time.Now().Unix(),
	}

	encodedMsg := encodeMessage(msg)
	maxRetries := 3
	timeout := time.Second * 5

	for attempt := 0; attempt < maxRetries; attempt++ {
		_, err := c.conn.Write(encodedMsg)
		if err != nil {
			return err
		}

		// Wait for ACK with timeout
		buffer := make([]byte, 1024)
		c.conn.SetReadDeadline(time.Now().Add(timeout))

		n, _, err := c.conn.ReadFromUDP(buffer)
		if err == nil && string(buffer[:n]) == fmt.Sprintf("ACK:%d", seqNum) {
			return nil
		}
	}

	return fmt.Errorf("failed to send message after %d attempts", maxRetries)
}

// Server implementation
type Server struct {
	conn           *net.UDPConn
	receivedSeqNos map[uint32]struct{}
	mu             sync.Mutex
}

func NewServer(port int) (*Server, error) {
	addr, err := net.ResolveUDPAddr("udp", fmt.Sprintf(":%d", port))
	if err != nil {
		return nil, err
	}

	conn, err := net.ListenUDP("udp", addr)
	if err != nil {
		return nil, err
	}

	return &Server{
		conn:           conn,
		receivedSeqNos: make(map[uint32]struct{}),
	}, nil
}

func (s *Server) Start() {
	buffer := make([]byte, 1024)
	for {
		n, remoteAddr, err := s.conn.ReadFromUDP(buffer)
		if err != nil {
			log.Println("Error reading:", err)
			continue
		}

		go s.handleMessage(buffer[:n], remoteAddr)
	}
}

func (s *Server) handleMessage(data []byte, remoteAddr *net.UDPAddr) {
	msg := decodeMessage(data)
	s.mu.Lock()
	defer s.mu.Unlock()

	if _, ok := s.receivedSeqNos[msg.SequenceNumber]; !ok {
		s.receivedSeqNos[msg.SequenceNumber] = struct{}{}
		log.Printf("Received message: Seq=%d, Payload=%s",
			msg.SequenceNumber, string(msg.Payload))
	}

	// Send/Resend ACK
	ackMsg := []byte(fmt.Sprintf("ACK:%d", msg.SequenceNumber))
	s.conn.WriteToUDP(ackMsg, remoteAddr)
}

// Proxy Server implementation
type ProxyServer struct {
	targetConn            *net.UDPConn
	conn                  *net.UDPConn
	packetLossProbability float64
	maxDelay              time.Duration
}

func NewProxyServer(port int, targetHost string, targetPort int, lossProbability float64, maxDelay time.Duration) (*ProxyServer, error) {
	targetAddr, err := net.ResolveUDPAddr("udp", fmt.Sprintf("%s:%d", targetHost, targetPort))
	if err != nil {
		return nil, err
	}

	targetConn, err := net.DialUDP("udp", nil, targetAddr)
	if err != nil {
		return nil, err
	}

	addr, err := net.ResolveUDPAddr("udp", fmt.Sprintf(":%d", port))
	if err != nil {
		return nil, err
	}

	conn, err := net.ListenUDP("udp", addr)
	if err != nil {
		return nil, err
	}

	return &ProxyServer{
		targetConn:            targetConn,
		conn:                  conn,
		packetLossProbability: lossProbability,
		maxDelay:              maxDelay,
	}, nil
}

func (p *ProxyServer) Start() {
	buffer := make([]byte, 1024)
	for {
		n, remoteAddr, err := p.conn.ReadFromUDP(buffer)
		if err != nil {
			log.Println("Proxy read error:", err)
			continue
		}

		go p.forwardPacket(buffer[:n], remoteAddr)
	}
}

func (p *ProxyServer) forwardPacket(data []byte, remoteAddr *net.UDPAddr) {
	// Simulate network conditions
	if rand.Float64() < p.packetLossProbability {
		log.Println("Packet to server dropped")
		return
	}

	// Actually forward the packet to the destination server
	_, err := p.targetConn.Write(data)
	if err != nil {
		log.Println("Proxy packet forwarding error:", err)
		return
	}

	buffer := make([]byte, 1024)
	n, _, err := p.targetConn.ReadFromUDP(buffer)
	if err != nil {
		log.Println("Proxy packet forwarding error:", err)
		return
	}

	if rand.Float64() < p.packetLossProbability {
		log.Println("Packet to client dropped")
		return
	}

	_, err = p.conn.WriteToUDP(buffer[:n], remoteAddr)
	if err != nil {
		log.Println("Proxy packet forwarding error:", err)
		return
	}
}

// Example main function demonstrating usage
func main() {
	mode := flag.String("mode", "", "Mode: client, server, or proxy")
	port := flag.Int("port", 8080, "Port number")
	serverHost := flag.String("host", "localhost", "Host address")
	serverPort := flag.Int("server-port", 8081, "Server port for proxy")
	lossProbability := flag.Float64("loss", 0.3, "Packet loss probability")
	flag.Parse()

	switch *mode {
	case "client":
		fmt.Println("Starting client")
		client, err := NewClient(*serverHost, *serverPort)
		if err != nil {
			log.Fatal(err)
		}
		defer client.conn.Close()

		// Read messages from stdin in a loop
		scanner := bufio.NewScanner(os.Stdin)
		fmt.Println("Enter messages (type 'exit' to quit):")

		for scanner.Scan() {
			message := scanner.Text()

			// Exit condition
			if message == "exit" {
				fmt.Println("Exiting client")
				break
			}

			// Send the message
			err = client.SendMessage([]byte(message))
			if err != nil {
				log.Println("Failed to send message:", err)
			} else {
				fmt.Println("Message sent successfully")
			}
		}

		if err := scanner.Err(); err != nil {
			log.Println("Error reading standard input:", err)
		}

	case "server":
		fmt.Println("Starting server")
		server, err := NewServer(*port)
		if err != nil {
			log.Fatal(err)
		}
		server.Start()

	case "proxy":
		fmt.Println("Starting proxy")
		proxy, err := NewProxyServer(*port, *serverHost, *serverPort, *lossProbability, time.Second)
		if err != nil {
			log.Fatal(err)
		}
		proxy.Start()

	default:
		fmt.Println("Please specify a mode: client, server, or proxy")
		os.Exit(1)
	}
}
