package cmd

import (
	"bufio"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"runtime"
	"strings"

	"github.com/ethan605/aws-vpn-client/pkg/samlserver"
	"golang.org/x/net/html"
)

const (
	defaultOvpnBin  = "./openvpn"
	defaultOvpnConf = "./ovpn.conf"
)

var (
	whitelistedLogPrefixes = []string{
		//"AUTH: Received control message: AUTH_FAILED,CRV1:R:",
		//"net_addr_v4_add",
		//"net_route_v4_add",
		//"/sbin/route add -net",
		//"Initialization Sequence Completed",
	}
)

// Cmd provides methods to connect to VPN using OpenVPN
type Cmd interface {
	ConnectVPN() error
}

// ParseConfigs parses and return a Cmd instance
func ParseConfigs() Cmd {
	configs := &cmdConfigs{
		stdoutCh: make(chan string),
	}

	flag.BoolVar(&configs.DebugMode, "debug", false, "debug mode")
	flag.StringVar(
		&configs.OnChallenge,
		"on-challenge",
		"",
		"auto (follow and parse challenge URL) or listen (spawn a SAML server and wait)",
	)
	flag.StringVar(&configs.OvpnBin, "ovpn", defaultOvpnBin, "path to OpenVPN binary")
	flag.StringVar(&configs.OvpnConf, "config", defaultOvpnConf, "path to OpenVPN config")
	flag.Parse()

	return configs
}

type cmdConfigs struct {
	DebugMode   bool
	OnChallenge string
	OvpnBin     string
	OvpnConf    string

	stdoutCh chan string
}

func (c *cmdConfigs) ConnectVPN() error {
	remoteIP := c.digRemoteIP()

	// Start SAML server
	cleanupCh := make(chan bool)

	// Wait for OpenVPN success and clean up
	go func() {
		for {
			line := <-c.stdoutCh

			for _, prefix := range whitelistedLogPrefixes {
				if strings.Contains(line, prefix) {
					fmt.Println(line)
				}
			}

			if strings.Contains(line, "Invalid username or password") {
				log.Println("Connection rejected, please re-run to try again")
			}

			// OpenVPN connected successfully, clean up and stop listening
			if strings.Contains(line, "Initialization Sequence Completed") {
				log.Println("Successfully connected")
				cleanupCh <- true
				return
			}
		}
	}()

	challengeURL, vpnSID, err := c.getChallengeData(remoteIP)
	if err != nil {
		return err
	}

	samlResponse := ""

	switch c.OnChallenge {
	case "auto":
		samlResponse, err = c.resolveChallengeURL(challengeURL)
	case "listen":
		samlResponse, err = c.listenForSAMLResponse(challengeURL, cleanupCh)
	default:
		err = errors.New("invalid -challenge mode")
	}

	if err != nil {
		return err
	}

	vpnPassword := fmt.Sprintf("CRV1::%s::%s", vpnSID, samlResponse)

	_, err = c.execOpenVPN(remoteIP, vpnPassword, false)
	if err != nil {
		return err
	}

	return nil
}

func (c *cmdConfigs) getChallengeData(remoteIP string) (string, string, error) {
	stdout, err := c.execOpenVPN(remoteIP, "ACS::35001", true)

	if err != nil {
		return "", "", err
	}

	for _, line := range stdout {
		if strings.Contains(line, "AUTH_FAILED,CRV1:R") {
			sidIdx := strings.Index(line, "instance-")

			if sidIdx < 0 {
				return "", "", errors.New("no challenge data found")
			}

			urlIdx := strings.Index(line, "https://")

			if urlIdx < 0 {
				return "", "", errors.New("no challenge data found")
			}

			return line[urlIdx:], line[sidIdx : urlIdx-9], nil
		}
	}

	return "", "", errors.New("no challenge data found")
}

func (c *cmdConfigs) execOpenVPN(remoteIP string, password string, forChallengeURL bool) ([]string, error) {
	port := 443
	userPass := fmt.Sprintf(`<( printf "%%s\n%%s\n" "%s" "%s" )`, "N/A", password)

	openVPN := fmt.Sprintf(
		`%s --config %s --remote %s %d --auth-user-pass %s`,
		c.OvpnBin, c.OvpnConf, remoteIP, port, userPass,
	)
	cmd := exec.Command("bash", "-c", openVPN)

	if !forChallengeURL {
		cmd = exec.Command("sudo", "bash", "-c", openVPN)
	}

	stdout, _ := cmd.StdoutPipe()
	if err := cmd.Start(); err != nil {
		return []string{}, err
	}

	lines := c.readLines(stdout, c.stdoutCh)
	return lines, cmd.Wait()
}

func (c *cmdConfigs) digRemoteIP() string {
	// Parse remote server
	cmd := exec.Command("bash", "-c", fmt.Sprintf("grep 'remote ' %s | cut -d' ' -f2", c.OvpnConf))
	stdout, err := cmd.Output()
	if err != nil {
		log.Fatal(err)
	}

	remoteServer := strings.Replace(string(stdout), "\n", "", -1)

	if c.DebugMode {
		log.Println("Remote server:", remoteServer)
	}

	// Create random hex
	rand, err := generateRandHex()
	if err != nil {
		log.Fatal(err)
	}

	// Lookup for remote IP
	remoteHost := fmt.Sprintf("%s.%s", rand, remoteServer)
	remoteIP, err := lookupRemoteIP(remoteHost)

	if err != nil {
		log.Fatal(err)
	}

	if c.DebugMode {
		log.Println("Remote IP:", remoteIP)
	}

	return remoteIP
}

func (c *cmdConfigs) resolveChallengeURL(challengeURL string) (string, error) {
	cookie := os.Getenv("CHALLENGE_URL_COOKIE")
	if cookie == "" {
		return "", errors.New("missing env var CHALLENGE_URL_COOKIE")
	}

	req, err := http.NewRequest("GET", challengeURL, nil)
	if err != nil {
		return "", err
	}

	req.Header.Add("Cookie", cookie)

	client := &http.Client{}
	res, err := client.Do(req)
	if err != nil {
		return "", err
	}

	// Read for <input value="..." />
	// which value is the SAML response
	token := html.NewTokenizer(res.Body)

	for {
		tokenType := token.Next()

		switch {
		case tokenType == html.ErrorToken:
			return "", errors.New("failed to parse SAML response HTML")

		case tokenType == html.StartTagToken:
			tag := token.Token()

			if tag.Data == "input" {
				value := ""

				for _, attr := range tag.Attr {
					if attr.Key == "value" {
						value = attr.Val
						break
					}
				}

				if value == "" {
					return "", errors.New("no SAML response value found")
				}

				return url.QueryEscape(value), nil
			}
		}
	}
}

func (c *cmdConfigs) listenForSAMLResponse(challengeURL string, cleanupCh <-chan bool) (string, error) {
	// Start SAML server
	server := samlserver.NewServer()
	go server.Run(cleanupCh)

	err := c.openChallengeURL(challengeURL)
	if err != nil {
		return "", err
	}

	return <-server.SAMLResponseCh(), nil
}

func (c *cmdConfigs) openChallengeURL(challengeURL string) error {
	openCmd := "xdg-open"

	if runtime.GOOS == "darwin" {
		openCmd = "open"
	}

	cmd := exec.Command(openCmd, challengeURL)
	if err := cmd.Start(); err != nil {
		return err
	}

	return nil
}

func (c *cmdConfigs) readLines(reader io.Reader, stdoutCh chan<- string) []string {
	lines := []string{}
	buf := bufio.NewReader(reader)

	for {
		line, _, err := buf.ReadLine()
		lineStr := string(line)

		if err != nil {
			break
		}

		if stdoutCh != nil {
			stdoutCh <- lineStr

			if c.DebugMode {
				log.Println(lineStr)
			}
		}

		lines = append(lines, lineStr)
	}

	return lines
}