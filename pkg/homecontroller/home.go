//homecontroller defines all handlerfucs in the root directory
package homecontroller

import (
	"fmt"
	"io/ioutil"
	"log"
	"net"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/jhoelzel/simpleapp/pkg/version"
)

func getUserIp(r *http.Request) string {
	var userIP string
	if len(r.Header.Get("CF-Connecting-IP")) > 1 {
		//Cloudflare
		userIP = r.Header.Get("CF-Connecting-IP")
	} else if len(r.Header.Get("X-Forwarded-For")) > 1 {
		//Forwarded
		userIP = r.Header.Get("X-Forwarded-For")
	} else if len(r.Header.Get("X-Real-IP")) > 1 {
		//Real IP set
		userIP = r.Header.Get("X-Real-IP")
	} else {
		userIP = r.RemoteAddr
		if strings.Contains(userIP, ":") {
			//If we have a port ignore it
			userIP = strings.Split(userIP, ":")[0]
		}
	}

	return userIP
}

// Get preferred outbound ip of this machine https://stackoverflow.com/questions/23558425/how-do-i-get-the-local-ip-address-in-go
func getOutboundIP() net.IP {
	conn, err := net.Dial("udp", "8.8.8.8:80")
	if err != nil {
		log.Fatal(err)
	}
	defer conn.Close()

	localAddr := conn.LocalAddr().(*net.UDPAddr)

	return localAddr.IP
}

// Get preferred outbound ip of this machine https://stackoverflow.com/questions/23558425/how-do-i-get-the-local-ip-address-in-go
func ConnectToSelf(secretIp string) string {
	var dialer net.Dialer
	dialer.Timeout = time.Second
	conn, err := dialer.Dial("udp", secretIp+":5060")
	if err != nil {
		return string(err.Error())
	}
	defer conn.Close()

	return "Connection successful"
}
func ConnectToSelfTcP(secretIp string) string {
	var dialer net.Dialer
	dialer.Timeout = time.Second
	_, err := dialer.Dial("tcp", secretIp+":5060")
	if err == nil {
		return "Connection successful"
	} else {
		return string(err.Error())
	}
}
func ConnectToSelfTcP61(secretIp string) string {
	var dialer net.Dialer
	dialer.Timeout = time.Second
	_, err := dialer.Dial("udp", secretIp+":5061")
	if err == nil {
		return "Connection successful"
	} else {
		return string(err.Error())
	}
}
func ConnectToSelfUdp61(secretIp string) string {
	var dialer net.Dialer
	dialer.Timeout = time.Second
	conn, err := dialer.Dial("udp", secretIp+":5061")
	if err != nil {
		return string(err.Error())
	}
	defer conn.Close()

	return "Connection successful"
}

// Get preferred outbound ip of this machine behind a nat https://www.codershood.info/2017/06/25/http-curl-request-golang/
func getOutBoundIPNat() string {
	url := "https://ifconfig.me/ip"

	req, err := http.NewRequest("GET", url, nil)

	res, err := http.DefaultClient.Do(req)
	if err != nil {
		return string(err.Error())
	}

	defer res.Body.Close()

	body, _ := ioutil.ReadAll(res.Body)

	return string(body)
}

// GetLocalIP returns the non loopback local IP of the host https://stackoverflow.com/questions/23558425/how-do-i-get-the-local-ip-address-in-go
func getLocalIP() string {
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		return ""
	}
	for _, address := range addrs {
		// check the address type and if it is not a loopback the display it
		if ipnet, ok := address.(*net.IPNet); ok && !ipnet.IP.IsLoopback() {
			if ipnet.IP.To4() != nil {
				return ipnet.IP.String()
			}
		}
	}
	return ""
}

//Home returns a simple HTTP handler function which writes a response containing current build info
func homeEndpoint(w http.ResponseWriter, r *http.Request) {
	w.WriteHeader(200)
	hostname, err := os.Hostname()
	if err != nil {
		http.Error(w, fmt.Sprint(err), http.StatusInternalServerError)
		return
	}
	keys, ok := r.URL.Query()["secretIp"]
	secretIp := "127.0.0.1"
	if !ok || len(keys[0]) < 1 {
		log.Println("Url Param 'key' is missing")

	} else {
		secretIp = keys[0]
	}
	w.Write([]byte("Welcome to the simpleapp test image!\n"))
	w.Write([]byte("------------------------------------------------\n"))
	w.Write([]byte("Running on container: " + hostname + "\n"))
	w.Write([]byte("The time is: " + time.Now().String() + "\n"))
	w.Write([]byte("The BuildTime is: " + version.BuildTime + "\n"))
	w.Write([]byte("The current Commit is: " + version.Commit + "\n"))
	w.Write([]byte("------------------------------------------------\n"))
	w.Write([]byte("The current User IP: " + getUserIp(r) + "\n"))
	w.Write([]byte("The current Outbound IP: " + getOutboundIP().String() + "\n"))
	w.Write([]byte("The current Outbound IP tested: \n"))
	for i := 1; i < 5; i++ {
		w.Write([]byte(getOutBoundIPNat() + "\n"))
	}
	w.Write([]byte("IP uses for connection: " + secretIp + "\n"))
	w.Write([]byte("UPD Connection port 5060: " + ConnectToSelf(secretIp) + "\n"))
	w.Write([]byte("UDP Connection port 5061: " + ConnectToSelfUdp61(secretIp) + "\n"))
	w.Write([]byte("TCP Connection port 5060: " + ConnectToSelfTcP(secretIp) + "\n"))
	w.Write([]byte("TCP Connection port 5061: " + ConnectToSelfTcP61(secretIp) + "\n"))
	w.Write([]byte("The current Local IP: " + getLocalIP() + "\n"))
	w.Write([]byte("------------------------------------------------\n"))

}
