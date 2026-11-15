package mirror

import (
	"context"
	"fmt"
	"net"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/bsdrip/winairplay/internal/airplay"
)

// airPlayPort is the TCP port on which an Apple TV accepts AirPlay.
const airPlayPort = 7000

// Scan finds AirPlay receivers without mDNS. It tries to connect to port
// 7000 of each address in the local IPv4 networks of this computer and asks
// each device that answers for its name. Use it when Discover finds nothing:
// some firewalls, VPN programs and Wi-Fi routers block mDNS.
//
// Only private networks are scanned, and at most the 254 addresses around
// the address of this computer in each network.
func Scan(ctx context.Context) []airplay.AirPlayDevice {
	return scanAddresses(ctx, localScanAddresses(), airPlayPort, 400*time.Millisecond)
}

// localScanAddresses lists the addresses to scan: the /24 network around
// each private IPv4 address of this computer, without the address itself.
func localScanAddresses() []string {
	interfaces, err := net.Interfaces()
	if err != nil {
		return nil
	}
	seen := map[string]bool{}
	var addresses []string
	for _, iface := range interfaces {
		if iface.Flags&net.FlagUp == 0 || iface.Flags&net.FlagLoopback != 0 {
			continue
		}
		addrs, err := iface.Addrs()
		if err != nil {
			continue
		}
		for _, addr := range addrs {
			ipNet, ok := addr.(*net.IPNet)
			if !ok {
				continue
			}
			ip := ipNet.IP.To4()
			if ip == nil || !ip.IsPrivate() {
				continue
			}
			prefix := fmt.Sprintf("%d.%d.%d.", ip[0], ip[1], ip[2])
			if seen[prefix] {
				continue
			}
			seen[prefix] = true
			for host := 1; host <= 254; host++ {
				if host != int(ip[3]) {
					addresses = append(addresses, fmt.Sprintf("%s%d", prefix, host))
				}
			}
		}
	}
	return addresses
}

func scanAddresses(ctx context.Context, addresses []string, port int, dialTimeout time.Duration) []airplay.AirPlayDevice {
	var (
		mu      sync.Mutex
		devices []airplay.AirPlayDevice
		wg      sync.WaitGroup
	)
	work := make(chan string)
	for worker := 0; worker < 64; worker++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for address := range work {
				if device, ok := probeReceiver(ctx, address, port, dialTimeout); ok {
					mu.Lock()
					devices = append(devices, device)
					mu.Unlock()
				}
			}
		}()
	}
feed:
	for _, address := range addresses {
		select {
		case work <- address:
		case <-ctx.Done():
			break feed
		}
	}
	close(work)
	wg.Wait()
	sortDevices(devices)
	return devices
}

// probeReceiver reports whether an AirPlay receiver answers at the address.
func probeReceiver(ctx context.Context, address string, port int, dialTimeout time.Duration) (airplay.AirPlayDevice, bool) {
	target := net.JoinHostPort(address, fmt.Sprint(port))
	dialer := net.Dialer{Timeout: dialTimeout}
	conn, err := dialer.DialContext(ctx, "tcp", target)
	if err != nil {
		return airplay.AirPlayDevice{}, false
	}
	_ = conn.Close()

	// The port is open. Ask for the receiver information. Something that is
	// not an AirPlay receiver gives no valid answer.
	type result struct {
		info *airplay.ReceiverInfo
		err  error
	}
	client := airplay.NewAirPlayClient(address, port)
	done := make(chan result, 1)
	go func() {
		if err := client.Connect(ctx); err != nil {
			done <- result{err: err}
			return
		}
		info, err := client.GetInfo()
		done <- result{info: info, err: err}
	}()
	var answer result
	select {
	case answer = <-done:
		_ = client.Close()
	case <-time.After(3 * time.Second):
		_ = client.Close()
		return airplay.AirPlayDevice{}, false
	case <-ctx.Done():
		_ = client.Close()
		return airplay.AirPlayDevice{}, false
	}
	if answer.err != nil || answer.info == nil || (answer.info.Name == "" && answer.info.DeviceID == "") {
		return airplay.AirPlayDevice{}, false
	}
	name := answer.info.Name
	if name == "" {
		name = address
	}
	return airplay.AirPlayDevice{
		Name:     name,
		Model:    answer.info.Model,
		IP:       address,
		Port:     port,
		DeviceID: answer.info.DeviceID,
	}, true
}

// sortDevices puts Apple TVs first and then sorts by name.
func sortDevices(devices []airplay.AirPlayDevice) {
	isAppleTV := func(d airplay.AirPlayDevice) bool { return strings.HasPrefix(d.Model, "AppleTV") }
	sort.SliceStable(devices, func(i, j int) bool {
		a, b := devices[i], devices[j]
		if isAppleTV(a) != isAppleTV(b) {
			return isAppleTV(a)
		}
		if !strings.EqualFold(a.Name, b.Name) {
			return strings.ToLower(a.Name) < strings.ToLower(b.Name)
		}
		return a.IP < b.IP
	})
}

// SameDevice reports whether two entries are the same receiver.
func SameDevice(a, b airplay.AirPlayDevice) bool {
	if a.DeviceID != "" && b.DeviceID != "" {
		return strings.EqualFold(a.DeviceID, b.DeviceID)
	}
	return a.IP == b.IP && a.Port == b.Port
}

// FindReceivers searches with mDNS and, when mDNS finds no receiver, scans
// the local networks. usedScan reports that the result came from the scan.
func FindReceivers(ctx context.Context, mdnsTimeout time.Duration) (devices []airplay.AirPlayDevice, usedScan bool, err error) {
	devices, err = Discover(ctx, mdnsTimeout)
	if len(devices) > 0 {
		return devices, false, nil
	}
	scanned := Scan(ctx)
	if len(scanned) > 0 {
		return scanned, true, nil
	}
	return nil, true, err
}
