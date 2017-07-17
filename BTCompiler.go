package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"github.com/gorilla/mux"
	"github.com/kardianos/osext"
	"io/ioutil"
	"net/http"
	"os"
	"os/exec"
	"path"
	"strings"
	"sync"
	"time"
)

const app = "BrewTroller Cloud Compiler Service"
const version = "1.0.0"

const SourceDir = "/BrewTroller"
const OptionsFileName = "/BrewTroller/options.json"

// Command line flags
var (
	debugMode  = flag.Bool("debug", false, "Enables server debug mode")
	pollPeriod = flag.Duration("poll", 5*time.Minute, "Github poll period")
	gitRepo    = flag.String("git", "http://github.com/brewtroller/brewtroller", "BrewTroller Remote Repository")
)

type BuildServer struct {
	version    string
	gitURL     string
	pollPeriod time.Duration

	execFolder string

	mu           sync.RWMutex //Protect the version tags and the source dir
	optionsCache map[string][]map[string]interface{}
}

func (bs *BuildServer) updateTags() {
	bs.mu.Lock()
	//clone the remote in a local repo
	localSrcDir := bs.execFolder + SourceDir
	os.RemoveAll(localSrcDir)

	cloneCmd := exec.Command("git", "clone", bs.gitURL, localSrcDir)
	_, err := cloneCmd.CombinedOutput()
	if err != nil {
		panic(err)
	}
	//Check if Source dir exists
	_, err = os.Stat(localSrcDir)
	if err != nil {
		panic("Could not create local source copy")
	}
	bs.mu.Unlock()

	for true {
		bs.mu.Lock()
		//Clear out all current tags, in case any have been removed
		clearCmd := exec.Command("git", "tag", "-l")
		clearCmd.Dir = localSrcDir
		removeCmd := exec.Command("xargs", "git", "tag", "-d")
		removeCmd.Dir = localSrcDir
		removeCmd.Stdin, _ = clearCmd.StdoutPipe()
		removeCmd.Start()
		clearCmd.Run()
		removeCmd.Wait()

		//Update the local repo
		pullCmd := exec.Command("git", "pull")
		pullCmd.Dir = localSrcDir
		pullCmd.Run()

		//get tag list
		tagCmd := exec.Command("git", "tag", "-l", "v[0-9]*\\.[0-9]*\\.[0-9]*")
		tagCmd.Dir = localSrcDir
		list, _ := tagCmd.Output()

		versionTags := strings.Split(string(list), "\n")
		//remove any blank tags
		for i := range versionTags {
			if strings.EqualFold(versionTags[i], "") {
				versionTags = append(versionTags[:i], versionTags[i+1:]...)
			}
		}
		//Build options cache
		bs.updateOptions(versionTags)

		bs.mu.Unlock()
		time.Sleep(bs.pollPeriod)
	}
}

// This method should only ever be called from within the poll worker, as it does not explicity lock the optionsCache itself
func (bs *BuildServer) updateOptions(versions []string) {

	optsManifest := make(map[string][]map[string]interface{})

	//parse the options manifest for each available version
	for _, ver := range versions {
		//checkout the version
		checkoutCmd := exec.Command("git", "checkout", ver)
		checkoutCmd.Dir = bs.execFolder + SourceDir
		checkoutCmd.Run()

		//parse the options file
		var opts, err = ioutil.ReadFile(bs.execFolder + OptionsFileName)
		if err != nil {
			// file doesn't exist, don't add version to manifest
			if *debugMode {
				fmt.Println("Options file for " + version + " does not exist, or cannot be opened!")
			}
			continue
		}
		var parsedOpts []map[string]interface{}
		err = json.Unmarshal(opts, &parsedOpts)
		if err != nil {
			if *debugMode {
				fmt.Println("Options file for " + version + " is invalid and cannot be parsed!")
			}
			continue
		}
		optsManifest[ver] = parsedOpts
	}
	//update BuildServer Options Cache
	bs.optionsCache = optsManifest
}

func NewServer(version string, gitUrl string, period time.Duration) *BuildServer {
	execFolder, _ := osext.ExecutableFolder()
	serv := &BuildServer{version: version, gitURL: gitUrl, pollPeriod: period, execFolder: execFolder}
	go serv.updateTags()
	return serv
}

func main() {
	flag.Parse()
	if *debugMode {
		fmt.Println("Debug mode enabled")
	}
	server := NewServer(version, *gitRepo, *pollPeriod)
	router := mux.NewRouter()
	router.HandleFunc("/", server.HomeHandler).Methods("GET")
	router.HandleFunc("/options", server.OptionsHandler).Methods("GET")
	router.HandleFunc("/build", server.BuildHandler).Methods("POST")
	http.ListenAndServe(":8080", router)
}

func makeErrorResonse(code string, err error, context ...string) []byte {
	em := make(map[string]string)

	em["code"] = code

	//If we are running in debug mode use the actual error as the message
	if *debugMode {
		em["message"] = err.Error()
	} else {
		//Not in debug mode, use generic response
		switch code {
		case "500":
			em["message"] = "Internal Server Error"
		case "400":
			em["message"] = "Bad Request"
		}
	}

	if *debugMode {
		for i, v := range context {
			em[fmt.Sprintf("context%i", i)] = v
		}
	}

	//Encode the error reponse for transmission
	enc, _ := json.Marshal(em)

	return enc
}

func (bs *BuildServer) HomeHandler(rw http.ResponseWriter, req *http.Request) {
	info := make(map[string]string)
	info["app"] = app
	info["version"] = version
	if *debugMode {
		c := exec.Command("uname", "-a")
		uname, _ := c.Output()
		info["host"] = string(uname)
	}
	rw.Header().Add("Access-Control-Allow-Origin", "*")
	rw.Header().Add("Content-Type", "application/json")
	encRes, _ := json.Marshal(info)
	rw.Write(encRes)
}

func (bs *BuildServer) OptionsHandler(rw http.ResponseWriter, req *http.Request) {
	bs.mu.RLock()
	opts, _ := json.Marshal(bs.optionsCache)
	bs.mu.RUnlock()

	rw.Header().Add("Access-Control-Allow-Origin", "*")
	rw.Header().Add("Content-Type", "application/json")
	rw.Write(opts)
}

func (bs *BuildServer) BuildHandler(rw http.ResponseWriter, req *http.Request) {
	//Generate a unique folder name to execute the build in
	// create a temp prefix with the requester addr, with '.' and ':' subbed
	reqID := strings.Replace(req.RemoteAddr, ".", "_", -1)
	reqID = strings.Replace(reqID, ":", "-", -1) + "-"
	tempDir, err := ioutil.TempDir("", reqID)

	//Handle error making temp build directory
	if err != nil {
		errResp := makeErrorResonse("500", err)
		rw.Header().Add("Access-Control-Allow-Origin", "*")
		rw.WriteHeader(http.StatusInternalServerError)
		rw.Write(errResp)
		return
	}
	//Clean-up the temp dir
	defer os.RemoveAll(tempDir)

	//Get request data
	reqData, err := ioutil.ReadAll(req.Body)

	//Handle error reading POST data
	if err != nil {
		errResp := makeErrorResonse("500", err)
		rw.Header().Add("Access-Control-Allow-Origin", "*")
		rw.WriteHeader(http.StatusInternalServerError)
		rw.Write(errResp)
		return
	}

	//Convert the post data to a map
	optsMap := make(map[string]interface{})
	err = json.Unmarshal(reqData, &optsMap)

	//Handle errors unmarshalling build options
	if err != nil {
		errResp := makeErrorResonse("400", err)
		rw.Header().Add("Access-Control-Allow-Origin", "*")
		rw.WriteHeader(http.StatusBadRequest)
		rw.Write(errResp)
		return
	}

	//Ensure we have a board option
	board, found := optsMap["board"].(string)
	if !found {
		err := errors.New("Board Option Must be Supplied!")
		errResp := makeErrorResonse("400", err)
		rw.Header().Add("Access-Control-Allow-Origin", "*")
		rw.WriteHeader(http.StatusBadRequest)
		rw.Write(errResp)
		return
	}

	//Ensure we have a build verison
	version, found := optsMap["BuildVersion"].(string)
	if !found {
		err := errors.New("Build Version Must be Supplied!")
		errResp := makeErrorResonse("400", err)
		rw.Header().Add("Access-Control-Allow-Origin", "*")
		rw.WriteHeader(http.StatusBadRequest)
		rw.Write(errResp)
		return
	}
	//Ensure that the build version is valid
	bs.mu.RLock()
	_, validVer := bs.optionsCache[version]
	bs.mu.RUnlock()
	if !validVer {
		err := errors.New("Build Version " + version + " is invalid!")
		errResp := makeErrorResonse("400", err)
		rw.Header().Add("Access-Control-Allow-Origin", "*")
		rw.WriteHeader(http.StatusBadRequest)
		rw.Write(errResp)
		return
	}

	//Remove the build version from the opts map, as CMake cannot use it
	delete(optsMap, "BuildVersion")

	//Make a slice to hold the options, with an init len of 0 and a capacity of 20
	//   we start with a capacity of 20 to prevent having to initialize a new slice after every append
	cmakeOpts := make([]string, 0, 20)
	//iterate through the build options requested and make a slice to pass to cmake
	for k, v := range optsMap {
		switch val := v.(type) {
        case string:
                opt := fmt.Sprintf("-D%s=%s", k, val)
                cmakeOpts = append(cmakeOpts, opt)
        case int:
                opt := fmt.Sprintf("-D%s=%d", k, val)
                cmakeOpts = append(cmakeOpts, opt)
        }
	}
	//Append the absolute path to the brewtroller source directory
	cmakeOpts = append(cmakeOpts, tempDir)

	//Clone the source repo into the temp dir
	pathToSource := bs.execFolder + SourceDir
	cloneCmd := exec.Command("git", "clone", pathToSource, tempDir)
	bs.mu.RLock()
	cloneCmd.Run()
	bs.mu.RUnlock()

	//Checkout the build version in the temp dir
	checkoutCmd := exec.Command("git", "checkout", version)
	checkoutCmd.Dir = tempDir
	checkoutCmd.Run()
	//Create the build dir
	buildDir := path.Join(tempDir, "/build")
	os.MkdirAll(buildDir, 0777)

        // Save copy of settings to build directory
        optionsPath := path.Join(tempDir,"user_config.json")

        err = ioutil.WriteFile(optionsPath, reqData, 0644)
        if err != nil {
               errResp := makeErrorResonse("500", err)
               rw.Header().Add("Access-Control-Allow-Origin", "*")
               rw.WriteHeader(http.StatusInternalServerError)
               rw.Write(errResp)
               return
        }

	//Attempt to setup Cmake build dir
	cmakeCmd := exec.Command("cmake", cmakeOpts...)
	cmakeCmd.Dir = buildDir

	cmakeOut, err := cmakeCmd.CombinedOutput()
	//Handle cmake setup error
	if err != nil {
		errResp := makeErrorResonse("500", err, string(cmakeOut))
		rw.Header().Add("Access-Control-Allow-Origin", "*")
		rw.WriteHeader(http.StatusInternalServerError)
		rw.Write(errResp)
		return
	}

	//build the image(s) -- in the future we will build an eeprom image to upload
	makeCmd := exec.Command("make")
	makeCmd.Dir = buildDir
	makeOut, err := makeCmd.CombinedOutput()
	//Handle any errors from make
	if err != nil {
		errResp := makeErrorResonse("500", err, string(makeOut))
		rw.Header().Add("Access-Control-Allow-Origin", "*")
		rw.WriteHeader(http.StatusInternalServerError)
		rw.Write(errResp)
		return
	}

	//Grab the binary and read it
	binary, err := ioutil.ReadFile(buildDir + "/src/BrewTroller-" + board + ".hex")
	if err != nil {
		errResp := makeErrorResonse("500", err)
		rw.Header().Add("Access-Control-Allow-Origin", "*")
		rw.WriteHeader(http.StatusInternalServerError)
		rw.Write(errResp)
		return
	}

	//Create response map
	resp := make(map[string]string)

	if *debugMode {
		resp["reqID"] = reqID
		resp["buildLocation"] = tempDir
		resp["reqDat"] = string(reqData)
		resp["cmake-output"] = string(cmakeOut)
		resp["make-output"] = string(makeOut)
	}

	resp["binary"] = string(binary)

	enc, _ := json.Marshal(resp)
	rw.Header().Add("Content-Type", "application/json")
	rw.Header().Add("Access-Control-Allow-Origin", "*")
	rw.Write(enc)
}
