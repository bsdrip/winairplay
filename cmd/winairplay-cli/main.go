// winairplay-cli mirrors the screen to an AirPlay receiver from the command
// line. It uses the same session code as the tray application and shows the
// full log, which helps to find problems.
package main

import (
	"bufio"
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"time"

	"github.com/bsdrip/winairplay/internal/airplay"
	"github.com/bsdrip/winairplay/internal/mirror"
)

func main() {
	list := flag.Bool("list", false, "List the AirPlay receivers on the network and exit")
	name := flag.String("name", "", "Receiver name, or a part of it (from -list)")
	target := flag.String("target", "", "Receiver IP address or host name (no discovery)")
	port := flag.Int("port", 7000, "AirPlay port, used with -target")
	code := flag.String("code", "", "PIN or password; $WINAIRPLAY_CODE has priority")
	pair := flag.Bool("pair", false, "Pair again, also when saved credentials exist")
	fps := flag.Int("fps", 30, "Frames per second")
	bitrate := flag.Int("bitrate", 0, "Video bitrate in kbps (0 = automatic)")
	encoder := flag.String("encoder", "auto", "H.264 encoder: auto, none (x264), nvenc, qsv, amf, mf, openh264")
	display := flag.Int("display", 0, "Monitor to capture, 0 = first")
	noCursor := flag.Bool("no-cursor", false, "Do not show the mouse cursor")
	test := flag.Bool("test", false, "Send a test pattern instead of the screen")
	ffmpeg := flag.String("ffmpeg", "", "Path of the FFmpeg program (default: next to this program, then PATH)")
	credBackend := flag.String("cred-backend", "", "Credential store: keyring or file (default: keyring on Windows)")
	credFile := flag.String("creds", "", "Credential file for -cred-backend file")
	debug := flag.Bool("debug", false, "Write the protocol debug log")
	flag.Parse()

	airplay.SetDebugMode(*debug)
	airplay.SetFFmpegPath(*ffmpeg)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	var device airplay.AirPlayDevice
	if *target != "" {
		device = airplay.AirPlayDevice{IP: *target, Port: *port}
	} else {
		fmt.Println("Searching for AirPlay receivers...")
		devices, err := mirror.Discover(ctx, 5*time.Second)
		if err != nil {
			log.Fatalf("discovery failed: %v", err)
		}
		if len(devices) == 0 {
			log.Fatal("no AirPlay receiver found; make sure that the PC and the Apple TV are on the same network")
		}
		for i, d := range devices {
			fmt.Printf("  [%d] %s (%s) %s:%d\n", i+1, d.Name, d.Model, d.IP, d.Port)
		}
		if *list {
			return
		}
		selected, err := selectDevice(devices, *name)
		if err != nil {
			log.Fatal(err)
		}
		device = *selected
	}

	credential := *code
	if env := os.Getenv("WINAIRPLAY_CODE"); env != "" {
		credential = env
	}
	stdin := bufio.NewReader(os.Stdin)
	ui := mirror.UI{
		AskCredential: func(prompt mirror.Prompt, receiver string) (string, bool) {
			switch prompt {
			case mirror.PromptPIN:
				fmt.Printf("Enter the PIN that %s shows: ", receiver)
			case mirror.PromptPassword:
				fmt.Printf("Enter the AirPlay password of %s: ", receiver)
			default:
				fmt.Printf("Enter the PIN or the password of %s: ", receiver)
			}
			line, err := stdin.ReadString('\n')
			line = strings.TrimRight(line, "\r\n")
			return line, err == nil || line != ""
		},
	}
	err := mirror.Run(ctx, device, mirror.Options{
		FPS: *fps, Bitrate: *bitrate, Encoder: *encoder, Display: *display,
		HideCursor: *noCursor, TestMode: *test, ForcePair: *pair,
		CredBackend: *credBackend, CredFile: *credFile, Code: credential,
	}, ui)
	if err != nil {
		log.Fatalf("error: %v", err)
	}
	log.Println("stopped")
}

func selectDevice(devices []airplay.AirPlayDevice, name string) (*airplay.AirPlayDevice, error) {
	if name != "" {
		var matches []*airplay.AirPlayDevice
		for i := range devices {
			if strings.EqualFold(devices[i].Name, name) {
				return &devices[i], nil
			}
			if strings.Contains(strings.ToLower(devices[i].Name), strings.ToLower(name)) {
				matches = append(matches, &devices[i])
			}
		}
		switch len(matches) {
		case 1:
			return matches[0], nil
		case 0:
			return nil, fmt.Errorf("no receiver has a name that contains %q", name)
		default:
			return nil, fmt.Errorf("%d receivers have a name that contains %q; give the full name", len(matches), name)
		}
	}
	if len(devices) == 1 {
		return &devices[0], nil
	}
	fmt.Print("Select a receiver [1]: ")
	var input string
	_, _ = fmt.Scanln(&input)
	input = strings.TrimSpace(input)
	if input == "" {
		return &devices[0], nil
	}
	index, err := strconv.Atoi(input)
	if err != nil || index < 1 || index > len(devices) {
		return nil, fmt.Errorf("selection %q is not valid", input)
	}
	return &devices[index-1], nil
}
