package main

import (
	"crypto/rand"
	"flag"
	"fmt"
	"github.com/danderson/tuntap"
	"gopkg.in/yaml.v2"
	"io/ioutil"
	"log"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"p2p/commons"
	"p2p/dht"
	//"p2p/enc"
	"p2p/udpcs"
	"sort"
	"strings"
	"time"
)

type MSG_TYPE uint16

// Main structure
type PTPCloud struct {
	// IP Address assigned to device at startup
	IP string

	// MAC Address assigned to device or generated by the application (TODO: Implement random generation and MAC assignment)
	Mac string

	HardwareAddr net.HardwareAddr

	// Netmask for device
	Mask string

	// Name of the device
	DeviceName string

	// Path to tool that is used to configure network device (only "ip" tools is supported at this moment)
	IPTool string `yaml:"iptool"`

	// TUN/TAP Interface
	Interface *os.File

	// Representation of TUN/TAP Device
	Device *tuntap.Interface

	NetworkPeers []NetworkPeer

	UDPSocket *udpcs.UDPClient

	LocalIPs []net.IP

	dht *dht.DHTClient
}

type NetworkPeer struct {
	// ID of the node received from DHT Bootstrap node
	ID string
	// Whether informaton about this node is filled or not
	// Normally it should be filled after peer-to-peer handshake procedure
	Unknown bool
	// This variables indicates whether handshake mechanism was started or not
	Handshaked bool
	// Clean address
	CleanAddr string
	// ID of the proxy used to communicate with the node
	ProxyID   int
	Forwarder *net.UDPAddr
	PeerAddr  *net.UDPAddr
	// IP of the peer we are connected to.
	PeerLocalIP net.IP
	// Hardware address of node's TUN/TAP device
	PeerHW net.HardwareAddr
	// Endpoint is the same as CleanAddr TODO: Remove CleanAddr
	Endpoint string
	// List of peer IP addresses
	KnownIPs []*net.UDPAddr
}

// Creates TUN/TAP Interface and configures it with provided IP tool
func (ptp *PTPCloud) CreateDevice(ip, mac, mask, device string) error {
	var err error

	ptp.IP = ip
	ptp.Mac = mac
	ptp.Mask = mask
	ptp.DeviceName = device

	// Extract necessary information from config file
	// TODO: Remove hard-coded path
	yamlFile, err := ioutil.ReadFile("config.yaml")
	if err != nil {
		log.Printf("[ERROR] Failed to load config: %v", err)
		ptp.IPTool = "/sbin/ip"
	}
	err = yaml.Unmarshal(yamlFile, ptp)
	if err != nil {
		log.Printf("[ERROR] Failed to parse config: %v", err)
		return err
	}

	ptp.Device, err = tuntap.Open(ptp.DeviceName, tuntap.DevTap)
	if ptp.Device == nil {
		log.Fatalf("[FATAL] Failed to open TAP device: %v", err)
		return err
	} else {
		log.Printf("[INFO] %v TAP Device created", ptp.DeviceName)
	}

	linkup := exec.Command(ptp.IPTool, "link", "set", "dev", ptp.DeviceName, "up")
	err = linkup.Run()
	if err != nil {
		log.Fatalf("[ERROR] Failed to up link: %v", err)
		return err
	}

	// Configure new device
	log.Printf("[INFO] Setting %s IP on device %s\n", ptp.IP, ptp.DeviceName)
	setip := exec.Command(ptp.IPTool, "addr", "add", ptp.IP+"/24", "dev", ptp.DeviceName)
	err = setip.Run()
	if err != nil {
		log.Fatalf("[FATAL] Failed to set IP: %v", err)
		return err
	}

	// Set MAC to device
	log.Printf("[INFO] Setting %s MAC on device %s\n", mac, ptp.DeviceName)
	setmac := exec.Command(ptp.IPTool, "link", "set", "dev", ptp.DeviceName, "address", mac)
	err = setmac.Run()
	if err != nil {
		log.Fatalf("[FATAL] Failed to set MAC: %v", err)
		return err
	}
	return nil
}

// Handles a packet that was received by TUN/TAP device
// Receiving a packet by device means that some application sent a network
// packet within a subnet in which our application works.
// This method calls appropriate gorouting for extracted packet protocol
func handlePacket(ptp *PTPCloud, contents []byte, proto int) {
	/*
		512   (PUP)
		2048  (IP)
		2054  (ARP)
		32821 (RARP)
		33024 (802.1q)
		34525 (IPv6)
		34915 (PPPOE discovery)
		34916 (PPPOE session)
	*/
	switch proto {
	case 512:
		log.Printf("[DEBUG] Received PARC Universal Packet")
		ptp.handlePARCUniversalPacket(contents)
	case 2048:
		ptp.handlePacketIPv4(contents, proto)
	case 2054:
		log.Printf("[DEBUG] Received ARP Packet")
		ptp.handlePacketARP(contents)
	case 32821:
		log.Printf("[DEBUG] Received RARP Packet")
		ptp.handleRARPPacket(contents)
	case 33024:
		log.Printf("[DEBUG] Received 802.1q Packet")
		ptp.handle8021qPacket(contents)
	case 34525:
		ptp.handlePacketIPv6(contents)
	case 34915:
		log.Printf("[DEBUG] Received PPPoE Discovery Packet")
		ptp.handlePPPoEDiscoveryPacket(contents)
	case 34916:
		log.Printf("[DEBUG] Received PPPoE Session Packet")
		ptp.handlePPPoESessionPacket(contents)
	default:
		log.Printf("[DEBUG] Received Undefined Packet")
	}
}

// Listen TAP interface for incoming packets
func (ptp *PTPCloud) ListenInterface() {
	// Read packets received by TUN/TAP device and send them to a handlePacket goroutine
	// This goroutine will decide what to do with this packet
	for {
		packet, err := ptp.Device.ReadPacket()
		if err != nil {
			log.Printf("Error reading packet: %s", err)
		}
		if packet.Truncated {
			log.Printf("[DEBUG] Truncated packet")
		}
		// TODO: Make handlePacket as a part of PTPCloud
		go handlePacket(ptp, packet.Packet, packet.Protocol)
	}
}

// This method will generate device name if none were specified at startup
func (ptp *PTPCloud) GenerateDeviceName(i int) string {
	var devName string = "vptp" + fmt.Sprintf("%d", i)
	inf, err := net.Interfaces()
	if err != nil {
		log.Printf("[ERROR] Failed to retrieve list of network interfaces")
		return ""
	}
	var exist bool = false
	for _, i := range inf {
		if i.Name == devName {
			exist = true
		}
	}
	if exist {
		return ptp.GenerateDeviceName(i + 1)
	} else {
		return devName
	}
}

// This method lists interfaces available in the system and retrieves their
// IP addresses
func (ptp *PTPCloud) FindNetworkAddresses() {
	log.Printf("[INFO] Looking for available network interfaces")
	inf, err := net.Interfaces()
	if err != nil {
		log.Printf("[ERROR] Failed to retrieve list of network interfaces")
		return
	}
	for _, i := range inf {
		addresses, err := i.Addrs()

		if err != nil {
			log.Printf("[ERROR] Failed to retrieve address for interface %v", err)
			continue
		}
		for _, addr := range addresses {
			var decision string = "Ignoring"
			var ipType string = "Unknown"
			ip, _, err := net.ParseCIDR(addr.String())
			if err != nil {
				log.Printf("[ERROR] Failed to parse CIDR notation: %v", err)
			}
			if ip.IsLoopback() {
				ipType = "Loopback"
			} else if ip.IsMulticast() {
				ipType = "Multicast"
			} else if ip.IsGlobalUnicast() {
				decision = "Saving"
				ipType = "Global Unicast"
			} else if ip.IsLinkLocalUnicast() {
				ipType = "Link Local Unicast"
			} else if ip.IsLinkLocalMulticast() {
				ipType = "Link Local Multicast"
			} else if ip.IsInterfaceLocalMulticast() {
				ipType = "Interface Local Multicast"
			}
			log.Printf("[INFO] Interface %s: %s. Type: %s. %s", i.Name, addr.String(), ipType, decision)
			if decision == "Saving" {
				ptp.LocalIPs = append(ptp.LocalIPs, ip)
			}
		}
	}
	log.Printf("[INFO] %d interfaces were saved", len(ptp.LocalIPs))
}

func main() {
	// TODO: Move this to init() function
	var (
		argIp      string
		argMask    string
		argMac     string
		argDev     string
		argDirect  string
		argHash    string
		argDht     string
		argKeyfile string
	)

	flag.StringVar(&argIp, "ip", "none", "IP Address to be used")
	// TODO: Parse this properly
	flag.StringVar(&argMask, "mask", "255.255.255.0", "Network mask")
	flag.StringVar(&argMac, "mac", "none", "MAC Address for a TUN/TAP interface")
	flag.StringVar(&argDev, "dev", "", "TUN/TAP interface name")
	// TODO: Direct connection is not implemented yet
	flag.StringVar(&argDirect, "direct", "none", "IP to connect to directly")
	flag.StringVar(&argHash, "hash", "none", "Infohash for environment")
	flag.StringVar(&argDht, "dht", "", "Specify DHT bootstrap node address")
	flag.StringVar(&argKeyfile, "key", "", "Path to yaml file containing crypto key")

	flag.Parse()
	if argIp == "none" {
		fmt.Println("USAGE: p2p [OPTIONS]")
		fmt.Printf("\nOPTIONS:\n")
		flag.PrintDefaults()
		os.Exit(1)
	}

	var hw net.HardwareAddr

	if argMac != "none" {
		var err2 error
		hw, err2 = net.ParseMAC(argMac)
		if err2 != nil {
			log.Printf("[ERROR] Invalid MAC address provided: %v", err2)
			return
		}
	} else {
		argMac, hw = GenerateMAC()
		log.Printf("[INFO] Generate MAC for TAP device: %s", argMac)
	}

	var crypter Crypto
	if argKeyfile != "" {
		crypter.ReadKeysFromFile(argKeyfile)
	}

	// Create new DHT Client, configured it and initialize
	// During initialization procedure, DHT Client will send
	// a introduction packet along with a hash to a DHT bootstrap
	// nodes that was hardcoded into it's code
	dhtClient := new(dht.DHTClient)
	config := dhtClient.DHTClientConfig()
	config.NetworkHash = argHash

	ptp := new(PTPCloud)
	ptp.FindNetworkAddresses()
	ptp.HardwareAddr = hw

	if argDev == "" {
		argDev = ptp.GenerateDeviceName(1)
	}

	ptp.CreateDevice(argIp, argMac, argMask, argDev)
	ptp.UDPSocket = new(udpcs.UDPClient)
	ptp.UDPSocket.Init("", 0)
	port := ptp.UDPSocket.GetPort()
	log.Printf("[INFO] Started UDP Listener at port %d", port)
	config.P2PPort = port
	if argDht != "" {
		config.Routers = argDht
	}
	ptp.dht = dhtClient.Initialize(config, ptp.LocalIPs)

	go ptp.UDPSocket.Listen(ptp.HandleP2PMessage)

	// Capture SIGINT
	// This is used for development purposes only, but later we should consider updating
	// this code to handle signals
	c := make(chan os.Signal, 1)
	signal.Notify(c, os.Interrupt)

	go func() {
		for sig := range c {
			fmt.Println("Received signal: ", sig)
			os.Exit(0)
		}
	}()

	go ptp.ListenInterface()
	for {
		time.Sleep(3 * time.Second)
		ptp.dht.UpdatePeers()
		// Wait two seconds before synchronizing with catched peers
		time.Sleep(2 * time.Second)
		ptp.PurgePeers(dhtClient.LastCatch)
		newPeersNum := ptp.SyncPeers(dhtClient.LastCatch)
		if newPeersNum > 0 {
			ptp.IntroducePeers()
		}
	}
}

// This method sends information about himself to empty peers
// Empty peers is a peer that was not sent us information
// about his device
func (ptp *PTPCloud) IntroducePeers() {
	for i, peer := range ptp.NetworkPeers {
		// Skip if know this peer
		if !peer.Unknown {
			continue
		}
		// Skip if we don't have an endpoint address for this peer
		if peer.Endpoint == "" {
			continue
		}
		log.Printf("[DEBUG] Introducing to %s", peer.Endpoint)
		addr, err := net.ResolveUDPAddr("udp", peer.Endpoint)
		if err != nil {
			log.Printf("[ERROR] Failed to resolve UDP address during Introduction: %v", err)
			continue
		}
		ptp.NetworkPeers[i].PeerAddr = addr
		// Send introduction packet
		msg := ptp.PrepareIntroductionMessage(ptp.dht.ID)
		_, err = ptp.UDPSocket.SendMessage(msg, addr)
		if err != nil {
			log.Printf("[ERROR] Failed to send introduction to %s", addr.String())
		} else {
			log.Printf("[DEBUG] Introduction sent to %s", peer.Endpoint)
		}
	}
}

func (ptp *PTPCloud) PrepareIntroductionMessage(id string) *udpcs.P2PMessage {
	var intro string = id + "," + ptp.Mac + "," + ptp.IP
	msg := udpcs.CreateIntroP2PMessage(intro, 0)
	return msg
}

// This method goes over peers and removes obsolete ones
// Peer becomes obsolete when it goes out of DHT
func (ptp *PTPCloud) PurgePeers(catched []string) {
	var rem []int
	for i, peer := range ptp.NetworkPeers {
		var f bool = false
		for _, newPeer := range ptp.dht.Peers {
			if newPeer.ID == peer.ID {
				f = true
			}
		}
		if !f {
			rem = append(rem, i)
		}
	}
	for i := range rem {
		ptp.NetworkPeers = append(ptp.NetworkPeers[:i], ptp.NetworkPeers[i+1:]...)
	}
	return

	// TODO: Old Scheme. Remove it before release
	var remove []int
	for i, peer := range ptp.NetworkPeers {
		var found bool = false
		for _, addr := range catched {
			if addr == peer.CleanAddr {
				found = true
			}
		}
		if !found {
			remove = append(remove, i)
		}
	}
	sort.Sort(sort.Reverse(sort.IntSlice(remove)))
	for i := range remove {
		ptp.NetworkPeers = append(ptp.NetworkPeers[:i], ptp.NetworkPeers[i+1:]...)
	}
}

// This method takes a list of catched peers from DHT and
// adds every new peer into list of peers
// Returns amount of peers that has been added
func (ptp *PTPCloud) SyncPeers(catched []string) int {
	var count int = 0

	for _, id := range ptp.dht.Peers {
		if id.ID == "" {
			continue
		}
		var found bool = false
		for i, peer := range ptp.NetworkPeers {
			if peer.ID == id.ID {
				found = true
				// Check if know something new about this peer, e.g. new addresses were
				// assigned to it
				for _, ip := range id.Ips {
					if ip == "" || ip == "0" {
						continue
					}
					var ipFound bool = false
					for _, kip := range peer.KnownIPs {
						if kip.String() == ip {
							ipFound = true
						}
					}
					if !ipFound {
						log.Printf("[INFO] Adding new IP (%s) address to %s", ip, peer.ID)
						// TODO: Check IP parsing
						newIp, _ := net.ResolveUDPAddr("udp", ip)
						ptp.NetworkPeers[i].KnownIPs = append(ptp.NetworkPeers[i].KnownIPs, newIp)
					}
				}

				// Set and Endpoint from peers if no endpoint were set previously
				if peer.Endpoint == "" {
					// First we need to go over each network and see if some of addresses are inside LAN
					// TODO: Implement
					var failback bool = false
					interfaces, err := net.Interfaces()
					if err != nil {
						log.Printf("[ERROR] Failed to retrieve list of network interfaces")
						failback = true
					}

					for _, inf := range interfaces {
						if inf.Name == ptp.DeviceName {
							continue
						}
						addrs, _ := inf.Addrs()
						for _, addr := range addrs {
							_, network, _ := net.ParseCIDR(addr.String())
							for _, kip := range ptp.NetworkPeers[i].KnownIPs {
								if network.Contains(kip.IP) {
									ptp.NetworkPeers[i].Endpoint = kip.String()
								}
							}
						}
					}

					// If we've failed to find something that is really close to us, skip to global
					if peer.Endpoint == "" && failback && len(ptp.NetworkPeers[i].KnownIPs) > 0 {
						log.Printf("[DEBUG] Setting endpoint for %s to %s", peer.ID, ptp.NetworkPeers[i].KnownIPs[0].String())
						ptp.NetworkPeers[i].Endpoint = ptp.NetworkPeers[i].KnownIPs[0].String()
						// Increase counter so p2p package will send introduction
						count = count + 1
					}
				}
			}
		}
		if !found {
			log.Printf("[INFO] Adding new peer. Requesting peer address")
			var newPeer NetworkPeer
			newPeer.ID = id.ID
			newPeer.Unknown = true
			ptp.NetworkPeers = append(ptp.NetworkPeers, newPeer)
			ptp.dht.RequestPeerIPs(id.ID)
		}
	}
	return count

	// TODO: Old Scheme. Remove it before release
	var c int
	for _, id := range catched {
		var found bool = false
		for _, peer := range ptp.NetworkPeers {
			if peer.ID == id {
				found = true
			}
		}
		if !found {
			var newPeer NetworkPeer
			newPeer.ID = id
			newPeer.Unknown = true
			ptp.NetworkPeers = append(ptp.NetworkPeers, newPeer)
			ptp.dht.RequestPeerIPs(id)
			c = c + 1
		}
	}
	return c
}

// WriteToDevice writes data to created TUN/TAP device
func (ptp *PTPCloud) WriteToDevice(b []byte) {
	var p tuntap.Packet
	p.Protocol = 2054
	p.Truncated = false
	p.Packet = b
	if ptp.Device == nil {
		log.Printf("[ERROR] TUN/TAP Device not initialized")
		return
	}
	err := ptp.Device.WritePacket(&p)
	if err != nil {
		log.Printf("[ERROR] Failed to write to TUN/TAP device")
	}
}

func GenerateMAC() (string, net.HardwareAddr) {
	buf := make([]byte, 6)
	_, err := rand.Read(buf)
	if err != nil {
		log.Printf("[ERROR] Failed to generate MAC: %v", err)
		return "", nil
	}
	buf[0] |= 2
	mac := fmt.Sprintf("06:%02x:%02x:%02x:%02x:%02x", buf[1], buf[2], buf[3], buf[4], buf[5])
	hw, err := net.ParseMAC(mac)
	if err != nil {
		log.Printf("[ERROR] Corrupted MAC address generated: %v", err)
		return "", nil
	}
	return mac, hw
}

// AddPeer adds new peer into list of network participants. If peer was added previously
// information about him will be updated. If not, new entry will be added
func (ptp *PTPCloud) AddPeer(addr *net.UDPAddr, id string, ip net.IP, mac net.HardwareAddr) {
	var found bool = false
	for i, peer := range ptp.NetworkPeers {
		if peer.ID == id {
			found = true
			ptp.NetworkPeers[i].CleanAddr = addr.String()
			ptp.NetworkPeers[i].ID = id
			ptp.NetworkPeers[i].PeerAddr = addr
			ptp.NetworkPeers[i].PeerLocalIP = ip
			ptp.NetworkPeers[i].PeerHW = mac
			ptp.NetworkPeers[i].Unknown = false
			ptp.NetworkPeers[i].Handshaked = true
		}
	}
	if !found {
		var newPeer NetworkPeer
		newPeer.ID = id
		newPeer.CleanAddr = addr.String()
		newPeer.PeerAddr = addr
		newPeer.PeerLocalIP = ip
		newPeer.PeerHW = mac
		newPeer.Unknown = false
		newPeer.Handshaked = true
		ptp.NetworkPeers = append(ptp.NetworkPeers, newPeer)
	}
}

func (p *NetworkPeer) ProbeConnection() bool {
	return false
}

func (ptp *PTPCloud) ParseIntroString(intro string) (string, net.HardwareAddr, net.IP) {
	parts := strings.Split(intro, ",")
	if len(parts) != 3 {
		log.Printf("[ERROR] Failed to parse introduction stirng")
		return "", nil, nil
	}
	var id string
	id = parts[0]
	// Extract MAC
	mac, err := net.ParseMAC(parts[1])
	if err != nil {
		log.Printf("[ERROR] Failed to parse MAC address from introduction packet: %v", err)
		return "", nil, nil
	}
	// Extract IP
	ip := net.ParseIP(parts[2])
	if ip == nil {
		log.Printf("[ERROR] Failed to parse IP address from introduction packet")
		return "", nil, nil
	}

	return id, mac, ip
}

func (ptp *PTPCloud) IsPeerUnknown(addr *net.UDPAddr) bool {
	for _, peer := range ptp.NetworkPeers {
		if peer.CleanAddr == addr.String() {
			return peer.Unknown
		}
	}
	return true
}

// Handler for new messages received from P2P network
func (ptp *PTPCloud) HandleP2PMessage(count int, src_addr *net.UDPAddr, err error, rcv_bytes []byte) {
	if err != nil {
		log.Printf("[ERROR] P2P Message Handle: %v", err)
		return
	}

	buf := make([]byte, count)
	copy(buf[:], rcv_bytes[:])

	msg, des_err := udpcs.P2PMessageFromBytes(buf)
	if des_err != nil {
		log.Printf("[ERROR] P2PMessageFromBytes error: %v", des_err)
		return
	}
	var msgType commons.MSG_TYPE = commons.MSG_TYPE(msg.Header.Type)
	switch msgType {
	case commons.MT_INTRO:
		log.Printf("[DEBUG] Introduction message received: %s", string(msg.Data))
		// Don't do anything if we already know everything about this peer
		if !ptp.IsPeerUnknown(src_addr) {
			log.Printf("[DEBUG] We already know this peer. Skip")
			return
		}
		id, mac, ip := ptp.ParseIntroString(string(msg.Data))
		ptp.AddPeer(src_addr, id, ip, mac)
		msg := ptp.PrepareIntroductionMessage(ptp.dht.ID)
		_, err := ptp.UDPSocket.SendMessage(msg, src_addr)
		if err != nil {
			log.Printf("[ERROR] Failed to respond to introduction message: %v", err)
		}
	case commons.MT_NENC:
		log.Printf("[DEBUG] Received P2P Message")
		ptp.WriteToDevice(msg.Data)
	default:
		log.Printf("[ERROR] Unknown message received")
	}

	//log.Printf("processed message from %s, msg_data : %s\n", src_addr.String(), msg.Data)
}

func (ptp *PTPCloud) SendTo(dst net.HardwareAddr, msg *udpcs.P2PMessage) (int, error) {
	for _, peer := range ptp.NetworkPeers {
		if peer.PeerHW.String() == dst.String() {
			size, err := ptp.UDPSocket.SendMessage(msg, peer.PeerAddr)
			return size, err
		}
	}
	return 0, nil
}
