package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"time"

	log "github.com/sirupsen/logrus"

	"github.com/pkg/sftp"
	"golang.org/x/crypto/ssh"
)

type resultSet struct {
	LastCollection string      `json:"LAST_COLLECTED"`
	Value          interface{} `json:"VALUE"`
	Result         string      `json:"RESULT"`
}

type sftpTest struct {
	user       string
	host       string
	port       string
	remoteFile string
	privateKey string
}

// ResultSet represents json result of specific URL monitoring, such DB2monitor (by Lukas Dierze)
type ResultSet struct {
	LastCollection string      `json:"LAST_COLLECTED"`
	Value          interface{} `json:"VALUE"`
	Result         string      `json:"RESULT"`
	Message        interface{} `json:"MESSAGE"`
	Description    interface{} `json:"DESC"`
}

func (s sftpTest) makeTest() (resultSet, error) {
	res := resultSet{LastCollection: time.Now().Format("2006-01-02 15:04:05"),
		Value: fmt.Sprintf("%s@%s%s:%s", s.user, s.host, s.port, s.remoteFile),
	}
	// get host public key
	hostKey, err := getHostKey(s.host)
	if err != nil {
		res.Result = fmt.Sprintf("FAIL: %s", err)
		return res, err
	}
	config := &ssh.ClientConfig{
		User: s.user,
		Auth: []ssh.AuthMethod{
			publicKeyFile(s.privateKey),
		},
		// HostKeyCallback: ssh.InsecureIgnoreHostKey(),
		HostKeyCallback: ssh.FixedHostKey(hostKey),
	}
	// connect
	log.Println("Connecting...")
	conn, err := ssh.Dial("tcp", s.host+s.port, config)
	if err != nil {
		res.Result = fmt.Sprintf("FAIL: %s", err)
		return res, err
	}
	defer conn.Close()

	// create new SFTP client
	log.Println("Creating sftp client...")
	client, err := sftp.NewClient(conn)
	if err != nil {
		res.Result = fmt.Sprintf("FAIL: %s", err)
		return res, err
	}
	defer client.Close()

	// create destination file
	log.Println("Creating destination file...")
	dstFile, err := os.Create("file.txt")
	if err != nil {
		res.Result = fmt.Sprintf("FAIL: %s", err)
		return res, err
	}
	defer dstFile.Close()

	// open source file
	log.Println("Opening source file...")
	srcFile, err := client.Open(s.remoteFile)
	if err != nil {
		res.Result = fmt.Sprintf("FAIL: %s", err)
		return res, err
	}

	// copy source file to destination file
	log.Println("Copying source file to destination...")
	bytes, err := io.Copy(dstFile, srcFile)
	if err != nil {
		res.Result = fmt.Sprintf("FAIL: %s", err)
		return res, err
	}
	fmt.Printf("%d bytes copied\n", bytes)

	// flush in-memory copy
	err = dstFile.Sync()
	if err != nil {
		res.Result = fmt.Sprintf("FAIL: %s", err)
		return res, err
	}
	res.Result = fmt.Sprintf("RESULT_OK")
	return res, nil
}

var sftpTester sftpTest
var inProgress bool
var regSize *regexp.Regexp
var crit, fatal int

func main() {

	// user := "archiver"
	// remote := "158.177.122.45"
	// port := ":22"
	// file := "sftp-userKeys/test/archiver/id_rsa"
	// remoteFile := "/device/in/meta/TSPALFA_DEVICE_2018-10-17_100100_0001.csv"

	//sftpTester := sftpTest{}
	if os.Getenv("SFTPHOST") == "" || os.Getenv("SFTPFILE") == "" {
		log.Fatal("OS variables SFTPHOST or SFTPFILE are not set")
	}
	sftpTester.host = os.Getenv("SFTPHOST")
	sftpTester.remoteFile = os.Getenv("SFTPFILE")

	if os.Getenv("SFTPPORT") == "" {
		sftpTester.port = ":22"
	} else {
		sftpTester.port = os.Getenv("SFTPPORT")
	}
	if os.Getenv("SFTPKEY") == "" {
		sftpTester.privateKey = "sftp-userKeys/test/archiver/id_rsa"
	} else {
		sftpTester.privateKey = os.Getenv("SFTPKEY")
	}

	if os.Getenv("SFTPUSER") == "" {
		sftpTester.user = "archiver"
	} else {
		sftpTester.user = os.Getenv("SFTPUSER")
	}
	// size monitoring
	regSize = regexp.MustCompile(`(\S+)\s+(\S+)\s+(\S+)\s+(\S+)\s+(\S+)%$`)
	var e error
	crit, e = strconv.Atoi(os.Getenv("CRITFS"))
	if e != nil {
		crit = 91
	}
	fatal, e = strconv.Atoi(os.Getenv("FATALFS"))
	if e != nil {
		fatal = 96
	}
	log.Infof("Thresholds for FS size: critical: %d, fatal: %d ", crit, fatal)

	http.HandleFunc("/isRunning", isRunning) // set router
	http.HandleFunc("/size", getFSize)
	err := http.ListenAndServe(":8080", nil) // set listen port
	if err != nil {
		log.Fatal("ListenAndServe: ", err)
	}

}

func getFSize(w http.ResponseWriter, r *http.Request) {
	//cmdSize := fmt.Sprintf("echo df|sftp -i $HOME/app/%s %s@%s|tail -1", sftpTester.privateKey, sftpTester.user, sftpTester.host)
	cmdSize := "getSize.sh"

	out, err := exec.Command(cmdSize).CombinedOutput()
	if err != nil {
		log.Errorf("Error during running %q: %s", cmdSize, err.Error())
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	outStr := strings.TrimSpace(string(out))
	log.Infof("Out: %q", outStr)
	// trying to parse
	res := regSize.FindStringSubmatch(outStr)

	if len(res) != 6 {
		log.Errorf("Expected 6 elements, got %d by the string %q", len(res), outStr)
		http.Error(w, "Wrong output", http.StatusInternalServerError)
		return
	}
	v, err := strconv.Atoi(res[5])
	if err != nil {
		log.Errorf("Can't convert %s to integer", res[4])
		http.Error(w, "Can't convert value to integer", http.StatusInternalServerError)
		return
	}
	resSet := ResultSet{
		LastCollection: time.Now().Format("2006-01-02 15:04:05"),
		Description:    "SFTP filesystem size",
		Message:        fmt.Sprintf("Size: %s\nUsed %s\nFree: %s", res[1], res[2], res[3]),
		Value:          v,
	}

	switch {
	case v >= fatal:
		resSet.Result = "RESULT_FATAL"
		log.Infof("SFTP filesystem under or equal fatal threshold. Current usage is %d%, threshold is %d%. Free space %s", v, fatal, res[3])
	case v >= crit:
		resSet.Result = "RESULT_CRITICAL"
		log.Infof("SFTP filesystem under or equal critical threshold. Current usage is %d%, threshold is %d%. Free space %s", v, crit, res[3])
	default:
		resSet.Result = "RESULT_OK"
		log.Infof("SFTP filesystem OK. Current usage is %d%, Free space %s", v, res[3])

	}
	w.Header().Add("Content-Type:", "application/json")
	response, _ := json.Marshal([]ResultSet{resSet})
	fmt.Fprint(w, string(response))
}

func isRunning(w http.ResponseWriter, r *http.Request) {
	if inProgress {
		fmt.Fprint(w, "Another test in progress")
		return
	}
	r.ParseForm()
	if r.FormValue("verbose") == "true" {
		log.SetOutput(w)
	} else {
		log.SetOutput(os.Stdout)
	}
	inProgress = true
	log.Infof("Test is going to be made to %s@%s%s, using file %s and trying to download file %s", sftpTester.user, sftpTester.host, sftpTester.port, sftpTester.privateKey, sftpTester.remoteFile)
	resOne, err := sftpTester.makeTest()
	res := []resultSet{resOne}
	b, _ := json.Marshal(res)
	if err != nil {
		log.Error(err)

		fmt.Fprintln(w, string(b))
	} else {
		fmt.Fprint(w, string(b))
	}
	inProgress = false
}
