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
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
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
	conn           *net.UDPConn
	sequenceNumber atomic.Uint32
	maxRetries     int
	timeout        time.Duration
}

func NewClient(serverHost string, serverPort int, maxRetries int, timeout time.Duration) (*Client, error) {
	serverAddr, err := net.ResolveUDPAddr("udp", fmt.Sprintf("%s:%d", serverHost, serverPort))
	if err != nil {
		return nil, err
	}

	conn, err := net.DialUDP("udp", nil, serverAddr)
	if err != nil {
		return nil, err
	}

	return &Client{
		conn:       conn,
		maxRetries: maxRetries,
		timeout:    timeout,
	}, nil
}

func (c *Client) SendMessage(payload []byte) error {
	msg := Message{
		SequenceNumber: c.sequenceNumber.Add(1) - 1,
		Payload:        payload,
		Timestamp:      time.Now().Unix(),
	}

	encodedMsg := encodeMessage(msg)
	for attempt := 0; attempt < c.maxRetries; attempt++ {
		_, err := c.conn.Write(encodedMsg)
		if err != nil {
			return err
		}

		// Wait for ACK with timeout
		buffer := make([]byte, 1024)
		c.conn.SetReadDeadline(time.Now().Add(c.timeout))
		n, _, err := c.conn.ReadFromUDP(buffer)
		if err == nil && string(buffer[:n]) == fmt.Sprintf("ACK:%d", msg.SequenceNumber) {
			return nil
		}
	}

	return fmt.Errorf("failed to send message after %d attempts", c.maxRetries)
}

// Server implementation
type Server struct {
	conn           *net.UDPConn
	receivedSeqNos map[uint32]struct{}
	mu             sync.Mutex
}

func NewServer(ip string, port int) (*Server, error) {
	addr, err := net.ResolveUDPAddr("udp", fmt.Sprintf("%s:%d", ip, port))
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
	targetConn *net.UDPConn
	conn       *net.UDPConn

	mu                                                                 sync.RWMutex
	packetDropInbound, packetDropOutbound, delayInbound, delayOutbound float64
	delayTimeInbound, delayTimeOutbound                                time.Duration
}

type ProxyConfig struct {
	PacketDropInbound  *float64
	PacketDropOutbound *float64
	DelayInbound       *float64
	DelayOutbound      *float64
	DelayTimeInbound   *time.Duration
	DelayTimeOutbound  *time.Duration
}

func NewProxyServer(ip string, port int, targetIP string, targetPort int, initialConfig *ProxyConfig) (*ProxyServer, error) {
	targetAddr, err := net.ResolveUDPAddr("udp", fmt.Sprintf("%s:%d", targetIP, targetPort))
	if err != nil {
		return nil, err
	}

	targetConn, err := net.DialUDP("udp", nil, targetAddr)
	if err != nil {
		return nil, err
	}

	addr, err := net.ResolveUDPAddr("udp", fmt.Sprintf("%s:%d", ip, port))
	if err != nil {
		return nil, err
	}

	conn, err := net.ListenUDP("udp", addr)
	if err != nil {
		return nil, err
	}

	ps := &ProxyServer{
		targetConn:         targetConn,
		conn:               conn,
		packetDropInbound:  0.2,
		packetDropOutbound: 0.2,
		delayInbound:       0.2,
		delayOutbound:      0.2,
		delayTimeInbound:   1000 * time.Millisecond,
		delayTimeOutbound:  1000 * time.Millisecond,
	}

	if initialConfig != nil {
		if err := ps.UpdateConfig(*initialConfig); err != nil {
			return nil, err
		}
	}

	return ps, nil
}

func (p *ProxyServer) UpdateConfig(config ProxyConfig) error {
	p.mu.Lock()
	defer p.mu.Unlock()

	if config.PacketDropInbound != nil {
		if *config.PacketDropInbound < 0 || *config.PacketDropInbound > 1 {
			return fmt.Errorf("inbound packet drop rate must be between 0 and 1")
		}
		p.packetDropInbound = *config.PacketDropInbound
	}

	if config.PacketDropOutbound != nil {
		if *config.PacketDropOutbound < 0 || *config.PacketDropOutbound > 1 {
			return fmt.Errorf("outbound packet drop rate must be between 0 and 1")
		}
		p.packetDropOutbound = *config.PacketDropOutbound
	}

	if config.DelayInbound != nil {
		if *config.DelayInbound < 0 || *config.DelayInbound > 1 {
			return fmt.Errorf("inbound delay probability must be between 0 and 1")
		}
		p.delayInbound = *config.DelayInbound
	}

	if config.DelayOutbound != nil {
		if *config.DelayOutbound < 0 || *config.DelayOutbound > 1 {
			return fmt.Errorf("outbound delay probability must be between 0 and 1")
		}
		p.delayOutbound = *config.DelayOutbound
	}

	if config.DelayTimeInbound != nil {
		p.delayTimeInbound = *config.DelayTimeInbound
	}

	if config.DelayTimeOutbound != nil {
		p.delayTimeOutbound = *config.DelayTimeOutbound
	}

	log.Println("Proxy configuration updated")
	return nil
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
	p.mu.RLock()
	defer p.mu.RUnlock()

	// Simulate network conditions
	if rand.Float64() < p.packetDropInbound {
		log.Println("Inbound packet dropped")
		return
	}

	if rand.Float64() < p.delayInbound {
		log.Println("Inbound packet delayed")
		time.Sleep(p.delayTimeInbound)
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

	if rand.Float64() < p.packetDropOutbound {
		log.Println("Outbound packet dropped")
		return
	}

	if rand.Float64() < p.delayOutbound {
		log.Println("Outbound packet delayed")
		time.Sleep(p.delayTimeOutbound)
	}

	_, err = p.conn.WriteToUDP(buffer[:n], remoteAddr)
	if err != nil {
		log.Println("Proxy packet forwarding error:", err)
		return
	}
}

func (p *ProxyServer) StartConfigListener() {
	scanner := bufio.NewScanner(os.Stdin)
	fmt.Println("Proxy configuration commands:")
	fmt.Println("config <param> <value>")
	fmt.Println("Available params: client-drop, server-drop, client-delay, server-delay, client-delay-time, server-delay-time")
	fmt.Println("Type 'exit' to stop configuration mode")

	for scanner.Scan() {
		input := scanner.Text()
		if input == "exit" {
			break
		}

		parts := strings.Fields(input)
		if len(parts) != 3 || parts[0] != "config" {
			fmt.Println("Invalid command. Use: config <param> <value>")
			continue
		}

		param, valueStr := parts[1], parts[2]
		var config ProxyConfig

		switch param {
		case "client-drop":
			val, err := strconv.ParseFloat(valueStr, 64)
			if err != nil || val < 0 || val > 1 {
				fmt.Println("Invalid value. Must be float between 0 and 1")
				continue
			}
			config.PacketDropInbound = &val
		case "server-drop":
			val, err := strconv.ParseFloat(valueStr, 64)
			if err != nil || val < 0 || val > 1 {
				fmt.Println("Invalid value. Must be float between 0 and 1")
				continue
			}
			config.PacketDropOutbound = &val
		case "client-delay":
			val, err := strconv.ParseFloat(valueStr, 64)
			if err != nil || val < 0 || val > 1 {
				fmt.Println("Invalid value. Must be float between 0 and 1")
				continue
			}
			config.DelayInbound = &val
		case "server-delay":
			val, err := strconv.ParseFloat(valueStr, 64)
			if err != nil || val < 0 || val > 1 {
				fmt.Println("Invalid value. Must be float between 0 and 1")
				continue
			}
			config.DelayOutbound = &val
		case "client-delay-time":
			val, err := strconv.ParseInt(valueStr, 10, 64)
			if err != nil {
				fmt.Println("Invalid value. Must be integer milliseconds")
				continue
			}
			delayTime := time.Duration(val) * time.Millisecond
			config.DelayTimeInbound = &delayTime
		case "server-delay-time":
			val, err := strconv.ParseInt(valueStr, 10, 64)
			if err != nil {
				fmt.Println("Invalid value. Must be integer milliseconds")
				continue
			}
			delayTime := time.Duration(val) * time.Millisecond
			config.DelayTimeOutbound = &delayTime
		default:
			fmt.Println("Unknown parameter")
			continue
		}

		err := p.UpdateConfig(config)
		if err != nil {
			fmt.Println("Error updating configuration:", err)
		} else {
			fmt.Println("Configuration updated successfully")
		}
	}
}

func main() {
	mode := flag.String("mode", "", "Mode: client, server, or proxy")
	listenIP := flag.String("listen-ip", "0.0.0.0", "IP to bind")
	listenPort := flag.Int("listen-port", 8081, "Port to bind")
	targetIP := flag.String("target-ip", "0.0.0.0", "Target IP")
	targetPort := flag.Int("target-port", 8081, "Target port")
	maxRetries := flag.Int("max-retries", 3, "Maximum retry time")
	timeout := flag.Int("timeout", 5, "Timeout in seconds")
	clientDrop := flag.Float64("client-drop", 0.2, "Drop chance for inbound")
	serverDrop := flag.Float64("server-drop", 0.2, "Drop chance for outbound")
	clientDelay := flag.Float64("client-delay", 0.2, "Delay chance for inbound")
	serverDelay := flag.Float64("server-delay", 0.2, "Delay chance for outbound")
	clientDelayTime := flag.Int64("client-delay-time", 1000, "Delay time in milliseconds")
	serverDelayTime := flag.Int64("server-delay-time", 1000, "Delay time in milliseconds")
	flag.Parse()

	switch *mode {
	case "client":
		log.Println("Starting client")
		client, err := NewClient(*targetIP, *targetPort, *maxRetries, time.Duration(*timeout)*time.Second)
		if err != nil {
			log.Fatal(err)
		}

		defer client.conn.Close()
		// Read messages from stdin in a loop
		scanner := bufio.NewScanner(os.Stdin)
		fmt.Println("Enter messages (type 'exit' to quit)")

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
				log.Println("Message sent successfully")
			}
		}

		if err := scanner.Err(); err != nil {
			log.Println("Error reading standard input:", err)
		}

	case "server":
		log.Println("Starting server")
		server, err := NewServer(*listenIP, *listenPort)
		if err != nil {
			log.Fatal(err)
		}

		defer server.conn.Close()
		server.Start()

	case "proxy":
		log.Println("Starting proxy")
		initialConfig := &ProxyConfig{
			PacketDropInbound:  clientDrop,
			PacketDropOutbound: serverDrop,
			DelayInbound:       clientDelay,
			DelayOutbound:      serverDelay,
			DelayTimeInbound: func() *time.Duration {
				t := time.Duration(*clientDelayTime) * time.Millisecond
				return &t
			}(),
			DelayTimeOutbound: func() *time.Duration {
				t := time.Duration(*serverDelayTime) * time.Millisecond
				return &t
			}(),
		}
		proxy, err := NewProxyServer(*listenIP, *listenPort, *targetIP, *targetPort, initialConfig)
		if err != nil {
			log.Fatal(err)
		}
		defer proxy.conn.Close()
		defer proxy.targetConn.Close()

		// Start configuration listener in a separate goroutine
		go proxy.StartConfigListener()

		proxy.Start()

	default:
		fmt.Println("Please specify a mode: client, server, or proxy")
		os.Exit(1)
	}
}
