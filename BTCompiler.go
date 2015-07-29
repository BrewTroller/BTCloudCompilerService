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
	"strings"
)

const app = "BrewTroller Build Bot"
const version = "0.1.0"

const SourceDir = "/BrewTroller"
const OptionsFileName = "/BrewTroller/options.json"

// Look for a run in debug mode flag, default to off
var debugMode = flag.Bool("debug", false, "Enables server debug mode")

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

func main() {
	flag.Parse()
	if *debugMode {
		fmt.Println("Debug mode enabled")
	}
	router := mux.NewRouter()
	router.HandleFunc("/", HomeHandler).Methods("GET")
	router.HandleFunc("/options", OptionsHandler).Methods("GET")
	router.HandleFunc("/build", BuildHandler).Methods("POST")
	http.ListenAndServe(":8080", router)
}

func HomeHandler(rw http.ResponseWriter, req *http.Request) {
	info := make(map[string]string)
	info["app"] = app
	info["version"] = version
	if *debugMode {
		c := exec.Command("uname", "-a")
		uname, _ := c.Output()
		info["host"] = string(uname)
	}
	rw.Header().Add("Content-Type", "application/json")
	encRes, _ := json.Marshal(info)
	rw.Write(encRes)
}

func OptionsHandler(rw http.ResponseWriter, req *http.Request) {
	//Read options file
	currDir, _ := osext.ExecutableFolder()
	var opts, err = ioutil.ReadFile(currDir + OptionsFileName)
	if err != nil {
		rw.WriteHeader(http.StatusInternalServerError)
		errResp := makeErrorResonse("500", err)
		rw.Write(errResp)
	}
	rw.Header().Add("Content-Type", "application/json")
	rw.Write(opts)
}

func BuildHandler(rw http.ResponseWriter, req *http.Request) {
	//Generate a unique folder name to execute the build in
	// create a temp prefix with the requester addr, with '.' and ':' subbed
	reqID := strings.Replace(req.RemoteAddr, ".", "_", -1)
	reqID = strings.Replace(reqID, ":", "-", -1) + "-"
	tempDir, err := ioutil.TempDir("", reqID)

	//Handle error making temp build directory
	if err != nil {
		errResp := makeErrorResonse("500", err)
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
		rw.WriteHeader(http.StatusInternalServerError)
		rw.Write(errResp)
		return
	}

	//Convert the post data to a map
	optsMap := make(map[string]string)
	err = json.Unmarshal(reqData, &optsMap)

	//Handle errors unmarshalling build options
	if err != nil {
		errResp := makeErrorResonse("400", err)
		rw.WriteHeader(http.StatusBadRequest)
		rw.Write(errResp)
		return
	}

	//Ensure we have a board option
	board, found := optsMap["board"]
	if !found {
		err := errors.New("Board Option Must be Supplied!")
		errResp := makeErrorResonse("400", err)
		rw.WriteHeader(http.StatusBadRequest)
		rw.Write(errResp)
		return
	}

	//Make a slice to hold the options, with an init len of 0 and a capacity of 20
	//   we start with a capacity of 20 to prevent having to initialize a new slice after every append
	cmakeOpts := make([]string, 0, 20)
	//iterate through the build options requested and make a slice to pass to cmake
	for k, v := range optsMap {
		opt := fmt.Sprintf("-D%s=%s", k, v)
		cmakeOpts = append(cmakeOpts, opt)
	}
	//Append the absolute path to the brewtroller source directory
	currDir, _ := osext.ExecutableFolder()
	pathToSource := currDir + SourceDir
	cmakeOpts = append(cmakeOpts, pathToSource)

	//Attempt to setup Cmake build dir
	cmakeCmd := exec.Command("cmake", cmakeOpts...)
	cmakeCmd.Dir = tempDir

	cmakeOut, err := cmakeCmd.CombinedOutput()
	//Handle cmake setup error
	if err != nil {
		errResp := makeErrorResonse("500", err, string(cmakeOut))
		rw.WriteHeader(http.StatusInternalServerError)
		rw.Write(errResp)
		return
	}

	//build the image(s) -- in the future we will build an eeprom image to upload
	makeCmd := exec.Command("make")
	makeCmd.Dir = tempDir
	makeOut, err := makeCmd.CombinedOutput()
	//Handle any errors from make
	if err != nil {
		errResp := makeErrorResonse("500", err, string(makeOut))
		rw.WriteHeader(http.StatusInternalServerError)
		rw.Write(errResp)
		return
	}

	//Grab the binary and read it
	binary, err := ioutil.ReadFile(tempDir + "/src/BrewTroller-" + board + ".hex")
	if err != nil {
		errResp := makeErrorResonse("500", err)
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
	rw.Write(enc)
}
