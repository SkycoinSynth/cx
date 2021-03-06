package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/ioutil"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/theherk/viper"

	cxcore "github.com/SkycoinProject/cx/cx"
	"github.com/SkycoinProject/cx/cxgo/actions"
	api2 "github.com/SkycoinProject/cx/cxgo/api"
	"github.com/SkycoinProject/cx/cxgo/cxgo0"
	"github.com/SkycoinProject/cx/cxgo/parser"

	"github.com/SkycoinProject/cx-chains/src/api"
	"github.com/SkycoinProject/cx-chains/src/cipher"
	"github.com/SkycoinProject/cx-chains/src/cli"
	"github.com/SkycoinProject/cx-chains/src/coin"
	"github.com/SkycoinProject/cx-chains/src/fiber"
	"github.com/SkycoinProject/cx-chains/src/readable"
	"github.com/SkycoinProject/cx-chains/src/skycoin"
	"github.com/SkycoinProject/cx-chains/src/util/logging"
	"github.com/SkycoinProject/cx-chains/src/visor"
	"github.com/SkycoinProject/cx-chains/src/wallet"
)

const VERSION = "0.7.1"

var (
	logger          = logging.MustGetLogger("newcoin")
	apiClient       = &http.Client{Timeout: 10 * time.Second}
	genesisBlockURL = "http://127.0.0.1:%d/api/v1/block?seq=0"
	profile         *os.File
)

var (
	// ErrMissingProjectRoot is returned when the project root parameter is missing
	ErrMissingProjectRoot = errors.New("missing project root")
	// ErrMissingSecretKey is returned when genesis secret is missing when distributing coins
	ErrMissingSecretKey = errors.New("missing genesis secret key")

	genesisSignature = ""
)

func getJSON(url string, target interface{}) error {
	r, err := apiClient.Get(url)
	if err != nil {
		return err
	}
	defer r.Body.Close()

	return json.NewDecoder(r.Body).Decode(target)
}

func initCXBlockchain(initPrgrm []byte, coinname, seckey string) error {
	var err error

	// check that data.db does not exist
	// if it does, delete it
	userHome := actions.UserHome()
	dbPath := filepath.Join(userHome, "."+coinname, "data.db")
	if _, err := cxcore.CXStatFile(dbPath); err == nil {
		logger.Infof("deleting %s", dbPath)
		err = cxcore.CXRemoveFile(dbPath)
		if err != nil {
			return err
		}
	}

	if seckey == "" {
		return ErrMissingSecretKey
	}

	genesisSecKey, err := cipher.SecKeyFromHex(seckey)
	if err != nil {
		return err
	}

	configDir := os.Getenv("GOPATH") + "/src/github.com/SkycoinProject/cx/"
	configFile := "fiber.toml"
	configFilepath := filepath.Join(configDir, configFile)
	// check that the config file exists
	if _, err := cxcore.CXStatFile(configFilepath); os.IsNotExist(err) {
		return err
	}

	projectRoot := os.Getenv("GOPATH") + "/src/github.com/SkycoinProject/cx"
	if projectRoot == "" {
		return ErrMissingProjectRoot
	}
	if _, err := cxcore.CXStatFile(projectRoot); os.IsNotExist(err) {
		return err
	}

	coinFile := filepath.Join(projectRoot, fmt.Sprintf("cmd/%[1]s/%[1]s.go", coinname))
	if _, err := cxcore.CXStatFile(coinFile); os.IsNotExist(err) {
		return err
	}

	// get fiber params
	params, err := fiber.NewConfig(configFile, configDir)

	cmd := exec.Command("go", "run", filepath.Join(projectRoot, fmt.Sprintf("cmd/%[1]s/%[1]s.go", coinname)), "-block-publisher=true", fmt.Sprintf("-blockchain-secret-key=%s", seckey),
		"-disable-incoming", "-max-out-msg-len=134217929")

	var genesisSig string
	var genesisBlock readable.Block

	stdoutIn, _ := cmd.StdoutPipe()
	stderrIn, _ := cmd.StderrPipe()
	cmd.Start()

	// fetch genesisSig and genesisBlock
	go func() {
		defer cmd.Process.Kill()

		genesisSigRegex, err := regexp.Compile(`Genesis block signature=([0-9a-zA-Z]+)`)
		if err != nil {
			logger.Error("error in regexp for genesis block signature")
			logger.Error(err)
			return
		}

		scanner := bufio.NewScanner(stdoutIn)
		scanner.Split(bufio.ScanLines)
		for scanner.Scan() {

			m := scanner.Text()
			logger.Info("Scanner: " + m)
			if genesisSigRegex.MatchString(m) {
				genesisSigSubString := genesisSigRegex.FindStringSubmatch(m)
				genesisSig = genesisSigSubString[1]

				// get genesis block
				err = getJSON(fmt.Sprintf(genesisBlockURL, params.Node.WebInterfacePort), &genesisBlock)

				return
			}
		}
	}()

	go func() {
		scanner := bufio.NewScanner(stderrIn)
		scanner.Split(bufio.ScanLines)
		for scanner.Scan() {
			logger.Error(scanner.Text())
		}
	}()

	cmd.Wait()

	// check that we were able to get genesisSig and genesisUxID

	if genesisSig != "" && len(genesisBlock.Body.Transactions) != 0 {
		genesisSignature = genesisSig
		logger.Infof("genesis sig: %s", genesisSig)

		// -- create new skycoin daemon to inject distribution transaction -- //
		if err != nil {
			logger.Error("error getting fiber parameters")
			return err
		}

		// get node config
		params.Node.DataDirectory = fmt.Sprintf("$HOME/.%s", coinname)
		nodeConfig := skycoin.NewNodeConfig("", params.Node)

		// create a new fiber coin instance
		newcoin := skycoin.NewCoin(
			skycoin.Config{
				Node: nodeConfig,
			},
			logger,
		)

		// parse config values
		newcoin.ParseConfig(flag.CommandLine)

		// dconf := newcoin.ConfigureDaemon()
		vconf := newcoin.ConfigureVisor()

		userHome := actions.UserHome()
		dbPath := filepath.Join(userHome, "."+coinname, "data.db")

		// logger.Infof("opening visor db: %s", dconf.Visor.DBPath)
		logger.Infof("opening visor db: %s", dbPath)
		db, err := visor.OpenDB(dbPath, false)
		if err != nil {
			logger.Error("Error opening DB")
			return err
		}
		defer db.Close()

		vs, err := visor.New(vconf, db, nil)
		if err != nil {
			logger.Error("Error with NewVisor")
			return err
		}

		headSeq, _, err := vs.HeadBkSeq()
		if err != nil {
			logger.Error("Error with HeadBkSeq")
			return err
		} else if headSeq == 0 {
			if len(genesisBlock.Body.Transactions) != 0 {
				var tx coin.Transaction

				UxID := genesisBlock.Body.Transactions[0].Out[0].Hash
				output := cipher.MustSHA256FromHex(UxID)
				tx.PushInput(output)

				addr := cipher.MustDecodeBase58Address("TkyD4wD64UE6M5BkNQA17zaf7Xcg4AufwX")
				tx.PushOutput(addr, uint64(1e10), 10000, initPrgrm)

				seckeys := make([]cipher.SecKey, 1)
				seckey := genesisSecKey.Hex()
				seckeys[0] = cipher.MustSecKeyFromHex(seckey)
				tx.SignInputs(seckeys)

				tx.UpdateHeader()
				err = tx.Verify()

				if err != nil {
					logger.Panic(err)
				}

				_, _, _, err := vs.InjectUserTransaction(tx)
				if err != nil {
					panic(err)
				}
			} else {
				logger.Error("ERROR: len genesis block was zero")
			}
		} else {
			logger.Error("ERROR: headSeq not zero")
		}
	} else {
		logger.Error("error getting genesis block")
	}
	return err
}

func runNode(mode string, options cxCmdFlags) *exec.Cmd {
	switch mode {
	case "publisher":
		return exec.Command("cxcoin", "-enable-all-api-sets",
			"-block-publisher=true",
			"-localhost-only",
			"-disable-default-peers",
			"-custom-peers-file=localhost-peers.txt",
			"-download-peerlist=false",
			"-launch-browser=false",
			fmt.Sprintf("-blockchain-secret-key=%s", options.secKey),
			fmt.Sprintf("-genesis-address=%s", options.genesisAddress),
			fmt.Sprintf("-genesis-signature=%s", options.genesisSignature),
			fmt.Sprintf("-blockchain-public-key=%s", options.pubKey),
			"-max-txn-size-unconfirmed=134217728",
			"-max-txn-size-create-block=134217728",
			"-max-block-size=134217728",
			"-max-in-msg-len=134217929",
			"-max-out-msg-len=134217929", // I don't know why this value, but the logger stated a value >= than this is needed
		)
	case "peer":
		return exec.Command("cxcoin", "-enable-all-api-sets",
			"-localhost-only",
			"-disable-default-peers",
			"-custom-peers-file=localhost-peers.txt",
			"-download-peerlist=false",
			"-launch-browser=false",
			fmt.Sprintf("-genesis-address=%s", options.genesisAddress),
			fmt.Sprintf("-genesis-signature=%s", options.genesisSignature),
			fmt.Sprintf("-blockchain-public-key=%s", options.pubKey),
			// "-web-interface-port=$(expr $2 + 420)",
			fmt.Sprintf("-web-interface-port=%d", options.port+420),
			fmt.Sprintf("-port=%d", options.port),
			fmt.Sprintf("-data-dir=/tmp/%d", options.port),
			"-max-txn-size-unconfirmed=134217728",
			"-max-txn-size-create-block=134217728",
			"-max-block-size=134217728",
			"-max-in-msg-len=134217929",
			"-max-out-msg-len=134217929", // I don't know why this value, but the logger stated a value >= than this is needed
		)
	default:
		return nil
	}
}

// optionTokenize checks if the user wants to use CX to generate the lexer tokens
func optionTokenize(options cxCmdFlags, fileNames []string) {
	var r *os.File
	var w *os.File
	var err error

	if len(fileNames) == 0 {
		r = os.Stdin
	} else {
		sourceFilename := fileNames[0]
		if len(fileNames) > 1 {
			fmt.Fprintln(os.Stderr, "Multiple source files detected. Ignoring all except", sourceFilename)
		}
		r, err = cxcore.CXOpenFile(sourceFilename)
		if err != nil {
			fmt.Fprintln(os.Stderr, "Error reading:", sourceFilename, err)
			return
		}
		defer r.Close()
	}

	if options.compileOutput == "" {
		w = os.Stdout
	} else {
		tokenFilename := options.compileOutput
		w, err = cxcore.CXCreateFile(tokenFilename)
		if err != nil {
			fmt.Fprintln(os.Stderr, "Error writing:", tokenFilename, err)
			return
		}
		defer w.Close()
	}

	parser.Tokenize(r, w)
}

// optionGenWallet checks if the user wants to use CX to create a new wallet. If
// this is the case, a wallet is generated for a peer node.
func optionGenWallet(options cxCmdFlags) {
	if options.walletSeed == "" {
		fmt.Println("creating a wallet requires a seed provided with --wallet-seed")
		return
	}
	if options.walletId == "" {
		// Although there is a default walletId.
		// This error should only occur if the user intentionally provides an empty id.
		fmt.Println("creating a wallet requires an id provided with --wallet-id")
		return
	}

	wltOpts := wallet.Options{
		Label: "cxcoin",
		Seed:  options.walletSeed,
	}

	wlt, err := cli.GenerateWallet(options.walletId, wltOpts, 1)
	if err != nil {
		panic(err)
	}
	// To Do: This needs to be changed or any CX chains will constantly be destroyed after each reboot.
	err = wlt.Save("/tmp/6001/wallets/")
	if err != nil {
		panic(err)
	}

	wltJSON, err := json.MarshalIndent(wlt.Meta, "", "\t")
	if err != nil {
		panic(err)
	}

	// Printing JSON with wallet information
	fmt.Println(string(wltJSON))
}

// optionGenAddress checks if the user wants to use CX to generate a new wallet
// address. If this is the case, CX prints the wallet information to standard
// output.
func optionGenAddress(options cxCmdFlags) {
	// Create a random seed to create a temporary wallet.
	seed := cli.MakeAlphanumericSeed()
	wltOpts := wallet.Options{
		Label: "cxcoin",
		Seed:  seed,
	}

	// Generate temporary wallet.
	wlt, err := cli.GenerateWallet(wallet.NewWalletFilename(), wltOpts, 1)
	if err != nil {
		panic(err)
	}

	rw := wallet.NewReadableWallet(wlt)

	output, err := json.MarshalIndent(rw, "", "    ")
	if err != nil {
		panic(err)
	}

	// Print all the wallet data.
	fmt.Println(string(output))
}

// optionRunNode checks if the user wants to run an `options.publisherMode` or
// `options.peerMode` node for a CX chain. If it's the case, either a publisher
// or a peer node
func optionRunNode(options cxCmdFlags) {
	var cmd *exec.Cmd
	if options.publisherMode {
		cmd = runNode("publisher", options)
	} else if options.peerMode {
		cmd = runNode("peer", options)
	}

	stdoutIn, _ := cmd.StdoutPipe()
	stderrIn, _ := cmd.StderrPipe()
	cmd.Start()

	go func() {
		defer cmd.Process.Kill()

		scanner := bufio.NewScanner(stdoutIn)
		scanner.Split(bufio.ScanLines)
		for scanner.Scan() {
			m := scanner.Text()
			logger.Info("Scanner: " + m)
		}
	}()

	go func() {
		scanner := bufio.NewScanner(stderrIn)
		scanner.Split(bufio.ScanLines)
		for scanner.Scan() {
			logger.Error(scanner.Text())
		}
	}()
}

// lexerStep0 performs a first pass for the CX parser. Globals, packages and
// custom types are added to `cxgo0.PRGRM0`.
func lexerStep0(srcStrs, srcNames []string) int {
	var prePkg *cxcore.CXPackage
	parseErrors := 0

	reMultiCommentOpen := regexp.MustCompile(`/\*`)
	reMultiCommentClose := regexp.MustCompile(`\*/`)
	reComment := regexp.MustCompile("//")

	rePkg := regexp.MustCompile("package")
	rePkgName := regexp.MustCompile("(^|[\\s])package\\s+([_a-zA-Z][_a-zA-Z0-9]*)")
	reStrct := regexp.MustCompile("type")
	reStrctName := regexp.MustCompile("(^|[\\s])type\\s+([_a-zA-Z][_a-zA-Z0-9]*)?\\s")

	reGlbl := regexp.MustCompile("var")
	reGlblName := regexp.MustCompile("(^|[\\s])var\\s([_a-zA-Z][_a-zA-Z0-9]*)")

	reBodyOpen := regexp.MustCompile("{")
	reBodyClose := regexp.MustCompile("}")

	reImp := regexp.MustCompile("import")
	reImpName := regexp.MustCompile("(^|[\\s])import\\s+\"([_a-zA-Z][_a-zA-Z0-9/-]*)\"")

	StartProfile("1. packages/structs")
	// 1. Identify all the packages and structs
	for srcI, srcStr := range srcStrs {
		srcName := srcNames[srcI]
		StartProfile(srcName)

		reader := strings.NewReader(srcStr)
		scanner := bufio.NewScanner(reader)
		var commentedCode bool
		var lineno = 0
		for scanner.Scan() {
			line := scanner.Bytes()
			lineno++

			// Identify whether we are in a comment or not.
			commentLoc := reComment.FindIndex(line)
			multiCommentOpenLoc := reMultiCommentOpen.FindIndex(line)
			multiCommentCloseLoc := reMultiCommentClose.FindIndex(line)
			if commentedCode && multiCommentCloseLoc != nil {
				commentedCode = false
			}
			if commentedCode {
				continue
			}
			if multiCommentOpenLoc != nil && !commentedCode && multiCommentCloseLoc == nil {
				commentedCode = true
				continue
			}

			// At this point we know that we are *not* in a comment

			// 1a. Identify all the packages
			if loc := rePkg.FindIndex(line); loc != nil {
				if (commentLoc != nil && commentLoc[0] < loc[0]) ||
					(multiCommentOpenLoc != nil && multiCommentOpenLoc[0] < loc[0]) ||
					(multiCommentCloseLoc != nil && multiCommentCloseLoc[0] > loc[0]) {
					// then it's commented out
					continue
				}

				if match := rePkgName.FindStringSubmatch(string(line)); match != nil {
					if pkg, err := cxgo0.PRGRM0.GetPackage(match[len(match)-1]); err != nil {
						// then it hasn't been added
						newPkg := cxcore.MakePackage(match[len(match)-1])
						cxgo0.PRGRM0.AddPackage(newPkg)
						prePkg = newPkg
					} else {
						prePkg = pkg
					}
				}
			}

			// 1b. Identify all the structs
			if loc := reStrct.FindIndex(line); loc != nil {
				if (commentLoc != nil && commentLoc[0] < loc[0]) ||
					(multiCommentOpenLoc != nil && multiCommentOpenLoc[0] < loc[0]) ||
					(multiCommentCloseLoc != nil && multiCommentCloseLoc[0] > loc[0]) {
					// then it's commented out
					continue
				}

				if match := reStrctName.FindStringSubmatch(string(line)); match != nil {
					if prePkg == nil {
						println(cxcore.CompilationError(srcName, lineno),
							"No package defined")
					} else if _, err := cxgo0.PRGRM0.GetStruct(match[len(match)-1], prePkg.Name); err != nil {
						// then it hasn't been added
						strct := cxcore.MakeStruct(match[len(match)-1])
						prePkg.AddStruct(strct)
					}
				}
			}
		}
		StopProfile(srcName)
	} // for range srcStrs
	StopProfile("1. packages/structs")

	StartProfile("2. globals")
	// 2. Identify all global variables
	//    We also identify packages again, so we know to what
	//    package we're going to add the variable declaration to.
	for i, source := range srcStrs {
		StartProfile(srcNames[i])
		// inBlock needs to be 0 to guarantee that we're in the global scope
		var inBlock int
		var commentedCode bool

		scanner := bufio.NewScanner(strings.NewReader(source))
		for scanner.Scan() {
			line := scanner.Bytes()

			// we need to ignore function bodies
			// it'll also ignore struct declaration's bodies, but this doesn't matter
			commentLoc := reComment.FindIndex(line)

			multiCommentOpenLoc := reMultiCommentOpen.FindIndex(line)
			multiCommentCloseLoc := reMultiCommentClose.FindIndex(line)

			if commentedCode && multiCommentCloseLoc != nil {
				commentedCode = false
			}

			if commentedCode {
				continue
			}

			if multiCommentOpenLoc != nil && !commentedCode && multiCommentCloseLoc == nil {
				commentedCode = true
				// continue
			}

			// Identify all the package imports.
			if loc := reImp.FindIndex(line); loc != nil {
				if (commentLoc != nil && commentLoc[0] < loc[0]) ||
					(multiCommentOpenLoc != nil && multiCommentOpenLoc[0] < loc[0]) ||
					(multiCommentCloseLoc != nil && multiCommentCloseLoc[0] > loc[0]) {
					// then it's commented out
					continue
				}

				if match := reImpName.FindStringSubmatch(string(line)); match != nil {
					pkgName := match[len(match)-1]
					// Checking if `pkgName` already exists and if it's not a standard library package.
					if _, err := cxgo0.PRGRM0.GetPackage(pkgName); err != nil && !cxcore.IsCorePackage(pkgName) {
						// _, sourceCode, srcNames := ParseArgsForCX([]string{fmt.Sprintf("%s%s", SRCPATH, pkgName)}, false)
						_, sourceCode, fileNames := cxcore.ParseArgsForCX([]string{filepath.Join(cxcore.SRCPATH, pkgName)}, false)
						ParseSourceCode(sourceCode, fileNames)
					}
				}
			}

			// we search for packages at the same time, so we can know to what package to add the global
			if loc := rePkg.FindIndex(line); loc != nil {
				if (commentLoc != nil && commentLoc[0] < loc[0]) ||
					(multiCommentOpenLoc != nil && multiCommentOpenLoc[0] < loc[0]) ||
					(multiCommentCloseLoc != nil && multiCommentCloseLoc[0] > loc[0]) {
					// then it's commented out
					continue
				}

				if match := rePkgName.FindStringSubmatch(string(line)); match != nil {
					if pkg, err := cxgo0.PRGRM0.GetPackage(match[len(match)-1]); err != nil {
						// then it hasn't been added
						prePkg = cxcore.MakePackage(match[len(match)-1])
						cxgo0.PRGRM0.AddPackage(prePkg)
					} else {
						prePkg = pkg
					}
				}
			}

			if locs := reBodyOpen.FindAllIndex(line, -1); locs != nil {
				for _, loc := range locs {
					if !(multiCommentCloseLoc != nil && multiCommentCloseLoc[0] > loc[0]) {
						// then it's outside of a */, e.g. `*/ }`
						if (commentLoc == nil && multiCommentOpenLoc == nil && multiCommentCloseLoc == nil) ||
							(commentLoc != nil && commentLoc[0] > loc[0]) ||
							(multiCommentOpenLoc != nil && multiCommentOpenLoc[0] > loc[0]) ||
							(multiCommentCloseLoc != nil && multiCommentCloseLoc[0] < loc[0]) {
							// then we have an uncommented opening bracket
							inBlock++
						}
					}
				}
			}

			if locs := reBodyClose.FindAllIndex(line, -1); locs != nil {
				for _, loc := range locs {
					if !(multiCommentCloseLoc != nil && multiCommentCloseLoc[0] > loc[0]) {
						if (commentLoc == nil && multiCommentOpenLoc == nil && multiCommentCloseLoc == nil) ||
							(commentLoc != nil && commentLoc[0] > loc[0]) ||
							(multiCommentOpenLoc != nil && multiCommentOpenLoc[0] > loc[0]) ||
							(multiCommentCloseLoc != nil && multiCommentCloseLoc[0] < loc[0]) {
							// then we have an uncommented closing bracket
							inBlock--
						}
					}
				}
			}

			// we could have this situation: {var local i32}
			// but we don't care about this, as the later passes will throw an error as it's invalid syntax

			if loc := rePkg.FindIndex(line); loc != nil {
				if (commentLoc != nil && commentLoc[0] < loc[0]) ||
					(multiCommentOpenLoc != nil && multiCommentOpenLoc[0] < loc[0]) ||
					(multiCommentCloseLoc != nil && multiCommentCloseLoc[0] > loc[0]) {
					// then it's commented out
					continue
				}

				if match := rePkgName.FindStringSubmatch(string(line)); match != nil {
					if pkg, err := cxgo0.PRGRM0.GetPackage(match[len(match)-1]); err != nil {
						// it should be already present
						panic(err)
					} else {
						prePkg = pkg
					}
				}
			}

			// finally, if we read a "var" and we're in global scope, we add the global without any type
			// the type will be determined later on
			if loc := reGlbl.FindIndex(line); loc != nil {
				if (commentLoc != nil && commentLoc[0] < loc[0]) ||
					(multiCommentOpenLoc != nil && multiCommentOpenLoc[0] < loc[0]) ||
					(multiCommentCloseLoc != nil && multiCommentCloseLoc[0] > loc[0]) || inBlock != 0 {
					// then it's commented out or inside a block
					continue
				}

				if match := reGlblName.FindStringSubmatch(string(line)); match != nil {
					if _, err := prePkg.GetGlobal(match[len(match)-1]); err != nil {
						// then it hasn't been added
						arg := cxcore.MakeArgument(match[len(match)-1], "", 0)
						arg.Offset = -1
						arg.Package = prePkg
						prePkg.AddGlobal(arg)
					}
				}
			}
		}
		StopProfile(srcNames[i])
	}
	StopProfile("2. globals")

	StartProfile("3. cxgo0")
	// cxgo0.Parse(allSC)
	for i, source := range srcStrs {
		StartProfile(srcNames[i])
		source = source + "\n"
		if len(srcNames) > 0 {
			cxgo0.CurrentFileName = srcNames[i]
		}
		parseErrors += cxgo0.Parse(source)
		StopProfile(srcNames[i])
	}
	StopProfile("3. cxgo0")
	return parseErrors
}

func cleanupAndExit(exitCode int) {
	StopCPUProfile(profile)
	os.Exit(exitCode)
}

// ParseSourceCode takes a group of files representing CX `sourceCode` and
// parses it into CX program structures for `PRGRM`.
func ParseSourceCode(sourceCode []*os.File, fileNames []string) {
	cxgo0.PRGRM0 = actions.PRGRM

	// Copy the contents of the file pointers containing the CX source
	// code into sourceCodeCopy
	sourceCodeCopy := make([]string, len(sourceCode))
	for i, source := range sourceCode {
		tmp := bytes.NewBuffer(nil)
		io.Copy(tmp, source)
		sourceCodeCopy[i] = string(tmp.Bytes())
	}

	// We need to traverse the elements by hierarchy first add all the
	// packages and structs at the same time then add globals, as these
	// can be of a custom type (and it could be imported) the signatures
	// of functions and methods are added in the cxgo0.y pass
	parseErrors := 0
	if len(sourceCode) > 0 {
		parseErrors = lexerStep0(sourceCodeCopy, fileNames)
	}

	actions.PRGRM.SelectProgram()

	actions.PRGRM = cxgo0.PRGRM0
	if cxcore.FoundCompileErrors || parseErrors > 0 {
		cleanupAndExit(cxcore.CX_COMPILATION_ERROR)
	}

	// Adding global variables `OS_ARGS` to the `os` (operating system)
	// package.
	if osPkg, err := actions.PRGRM.GetPackage(cxcore.OS_PKG); err == nil {
		if _, err := osPkg.GetGlobal(cxcore.OS_ARGS); err != nil {
			arg0 := cxcore.MakeArgument(cxcore.OS_ARGS, "", -1).AddType(cxcore.TypeNames[cxcore.TYPE_UNDEFINED])
			arg0.Package = osPkg

			arg1 := cxcore.MakeArgument(cxcore.OS_ARGS, "", -1).AddType(cxcore.TypeNames[cxcore.TYPE_STR])
			arg1 = actions.DeclarationSpecifiers(arg1, []int{0}, cxcore.DECL_BASIC)
			arg1 = actions.DeclarationSpecifiers(arg1, []int{0}, cxcore.DECL_SLICE)

			actions.DeclareGlobalInPackage(osPkg, arg0, arg1, nil, false)
		}
	}

	StartProfile("4. parse")
	// The last pass of parsing that generates the actual output.
	for i, source := range sourceCodeCopy {
		// Because of an unkown reason, sometimes some CX programs
		// throw an error related to a premature EOF (particularly in Windows).
		// Adding a newline character solves this.
		source = source + "\n"
		actions.LineNo = 1
		b := bytes.NewBufferString(source)
		if len(fileNames) > 0 {
			actions.CurrentFile = fileNames[i]
		}
		StartProfile(actions.CurrentFile)
		parseErrors += parser.Parse(parser.NewLexer(b))
		StopProfile(actions.CurrentFile)
	}
	StopProfile("4. parse")

	if cxcore.FoundCompileErrors || parseErrors > 0 {
		cleanupAndExit(cxcore.CX_COMPILATION_ERROR)
	}
}

func parseProgram(options cxCmdFlags, fileNames []string, sourceCode []*os.File) (bool, []byte, []byte) {
	profile := StartCPUProfile("parse")
	defer StopCPUProfile(profile)

	defer DumpMEMProfile("parse")

	StartProfile("parse")
	defer StopProfile("parse")

	actions.PRGRM = cxcore.MakeProgram()
	corePkgsPrgrm, err := cxcore.GetProgram()
	if err != nil {
		panic(err)
	}
	actions.PRGRM.Packages = corePkgsPrgrm.Packages

	if options.webMode {
		ServiceMode()
		return false, nil, nil
	}

	// TODO @evanlinjin: Do we need this? What is the 'leaps' command?
	if options.ideMode {
		IdeServiceMode()
		ServiceMode()
		return false, nil, nil
	}

	// TODO @evanlinjin: We do not need a persistent mode?
	if options.webPersistentMode {
		go ServiceMode()
		PersistentServiceMode()
		return false, nil, nil
	}

	// TODO @evanlinjin: This is a separate command now.
	if options.tokenizeMode {
		optionTokenize(options, fileNames)
		return false, nil, nil
	}

	// var bcPrgrm *CXProgram
	var sPrgrm []byte
	// In case of a CX chain, we need to temporarily store the blockchain code heap elsewhere,
	// so we can then add it after the transaction code's data segment.
	var bcHeap []byte
	if options.transactionMode || options.broadcastMode {
		chainStatePrelude(&sPrgrm, &bcHeap, actions.PRGRM) // TODO: refactor injection logic
	}

	// Parsing all the source code files sent as CLI arguments to CX.
	ParseSourceCode(sourceCode, fileNames)

	// setting project's working directory
	if !options.replMode && len(sourceCode) > 0 {
		cxgo0.PRGRM0.Path = determineWorkDir(sourceCode[0].Name())
	}

	// Checking if a main package exists. If not, create and add it to `PRGRM`.
	if _, err := actions.PRGRM.GetFunction(cxcore.MAIN_FUNC, cxcore.MAIN_PKG); err != nil {
		initMainPkg(actions.PRGRM)
	}
	// Setting what function to start in if using the REPL.
	actions.ReplTargetFn = cxcore.MAIN_FUNC

	// Adding *init function that initializes all the global variables.
	addInitFunction(actions.PRGRM)

	actions.LineNo = 0

	if cxcore.FoundCompileErrors {
		cleanupAndExit(cxcore.CX_COMPILATION_ERROR)
	}

	return true, bcHeap, sPrgrm
}

func runProgram(options cxCmdFlags, cxArgs []string, sourceCode []*os.File, bcHeap []byte, sPrgrm []byte) {
	StartProfile("run")
	defer StopProfile("run")

	if options.replMode || len(sourceCode) == 0 {
		actions.PRGRM.SelectProgram()
		repl()
		return
	}

	// If it's a CX chain transaction, we need to add the heap extracted
	// from the retrieved CX chain program state.
	if options.transactionMode || options.broadcastMode {
		mergeBlockchainHeap(bcHeap, sPrgrm) // TODO: refactor injection logic
	}

	if options.blockchainMode {
		// Initializing the CX chain.
		err := actions.PRGRM.RunCompiled(0, cxArgs)
		if err != nil {
			panic(err)
		}

		actions.PRGRM.RemovePackage(cxcore.MAIN_FUNC)

		// Removing garbage from the heap. Only the global variables should be left
		// as these are independent from function calls.
		cxcore.MarkAndCompact(actions.PRGRM)
		actions.PRGRM.HeapSize = actions.PRGRM.HeapPointer

		// We already removed the main package, so it's
		// len(PRGRM.Packages) instead of len(PRGRM.Packages) - 1.
		actions.PRGRM.BCPackageCount = len(actions.PRGRM.Packages)
		s := cxcore.Serialize(actions.PRGRM, actions.PRGRM.BCPackageCount)
		s = cxcore.ExtractBlockchainProgram(s, s)

		configDir := os.Getenv("GOPATH") + "/src/github.com/SkycoinProject/cx/"
		configFile := "fiber"

		cmd := exec.Command("go", "install", "./cmd/newcoin/...")
		cmd.Start()
		cmd.Wait()

		cmd = exec.Command("newcoin", "createcoin",
			fmt.Sprintf("--coin=%s", options.programName),
			fmt.Sprintf("--template-dir=%s%s", os.Getenv("GOPATH"), "/src/github.com/SkycoinProject/cx/template"),
			"--config-file="+configFile+".toml",
			"--config-dir="+configDir,
		)
		cmd.Start()
		cmd.Wait()

		cmd = exec.Command("go", "install", "./cmd/cxcoin/...")
		cmd.Start()
		cmd.Wait()

		err = initCXBlockchain(s, options.programName, options.secKey)
		if err != nil {
			panic(err)
		}
		fmt.Println("\ngenesis signature:", genesisSignature)

		viper.SetConfigName(configFile) // name of config file (without extension)
		viper.AddConfigPath(".")        // optionally look for config in the working directory
		err = viper.ReadInConfig()      // Find and read the config file
		if err != nil {                 // Handle errors reading the config file
			panic(err)
		}

		viper.Set("node.genesis_signature_str", genesisSignature)
		viper.WriteConfig()

		cmd = exec.Command("newcoin", "createcoin",
			fmt.Sprintf("--coin=%s", options.programName),
			fmt.Sprintf("--template-dir=%s%s", os.Getenv("GOPATH"), "/src/github.com/SkycoinProject/cx/template"),
			"--config-file="+configFile+".toml",
			"--config-dir="+configDir,
		)
		cmd.Start()
		cmd.Wait()
		cmd = exec.Command("go", "install", "./cmd/cxcoin/...")
		cmd.Start()
		cmd.Wait()
	} else if options.broadcastMode {
		// Setting the CX runtime to run `PRGRM`.
		actions.PRGRM.SelectProgram()
		cxcore.MarkAndCompact(actions.PRGRM)

		s := cxcore.Serialize(actions.PRGRM, actions.PRGRM.BCPackageCount)
		txnCode := cxcore.ExtractTransactionProgram(sPrgrm, s)

		// All these HTTP requests need to be dropped in favor of calls to calls to functions
		// from the `cli` or `api` Skycoin packages
		addr := fmt.Sprintf("http://127.0.0.1:%d", options.port+420)
		skycoinClient := api.NewClient(addr)
		csrfToken, err := skycoinClient.CSRF()
		if err != nil {
			panic(err)
		}

		url := fmt.Sprintf("http://127.0.0.1:%d/api/v1/wallet/transaction", options.port+420)

		var dataMap map[string]interface{}
		dataMap = make(map[string]interface{}, 0)
		dataMap["mainExprs"] = txnCode
		dataMap["hours_selection"] = map[string]string{"type": "manual"}
		// dataMap["wallet_id"] = map[string]string{"id": options.walletId}
		dataMap["wallet_id"] = string(options.walletId)
		dataMap["to"] = []interface{}{map[string]string{"address": "2PBcLADETphmqWV7sujRZdh3UcabssgKAEB", "coins": "1", "hours": "0"}}

		jsonStr, err := json.Marshal(dataMap)
		if err != nil {
			panic(err)
		}

		req, err := http.NewRequest("POST", url, bytes.NewBuffer(jsonStr))
		req.Header.Set("X-CSRF-Token", csrfToken)
		req.Header.Set("Content-Type", "application/json")

		client := &http.Client{}
		resp, err := client.Do(req)
		if err != nil {
			panic(err)
		}

		defer resp.Body.Close()
		body, err := ioutil.ReadAll(resp.Body)
		if err != nil {
			panic(err)
		}

		var respBody map[string]interface{}
		if err := json.Unmarshal(body, &respBody); err != nil {
			// Printing the body instead of `err`. Body has the error generated in the Skycoin API.
			fmt.Println(string(body))
			return
		}

		url = fmt.Sprintf("http://127.0.0.1:%d/api/v1/injectTransaction", options.port+420)
		dataMap = make(map[string]interface{}, 0)
		dataMap["rawtx"] = respBody["encoded_transaction"]

		jsonStr, err = json.Marshal(dataMap)
		if err != nil {
			panic(err)
		}

		req, err = http.NewRequest("POST", url, bytes.NewBuffer(jsonStr))
		req.Header.Set("X-CSRF-Token", csrfToken)
		req.Header.Set("Content-Type", "application/json")

		resp, err = client.Do(req)
		if err != nil {
			panic(err)
		}

		body, err = ioutil.ReadAll(resp.Body)
		if err != nil {
			panic(err)
		}
	} else {
		// Normal run of a CX program.
		err := actions.PRGRM.RunCompiled(0, cxArgs)
		if err != nil {
			panic(err)
		}

		if cxcore.AssertFailed() {
			os.Exit(cxcore.CX_ASSERT)
		}
	}
}

func Run(args []string) {
	runtime.LockOSThread()
	runtime.GOMAXPROCS(2)

	options := defaultCmdFlags()
	parseFlags(&options, args)

	// Checking if CXPATH is set, either by setting an environment variable
	// or by setting the `--cxpath` flag.
	checkCXPathSet(options)

	// Does the user want to run a CX publisher or peer node?
	if options.publisherMode || options.peerMode {
		optionRunNode(options)
	}
	// Does the user want to generate a new wallet address?
	if options.genAddress {
		optionGenAddress(options)
		return
	}
	// Does the user want to generate a new wallet address?
	if options.walletMode {
		optionGenWallet(options)
		return
	}
	// Does the user want to print the command-line help?
	if options.printHelp {
		printHelp()
		return
	}
	// Does the user want to print CX's version?
	if options.printVersion {
		printVersion()
		return
	}

	if options.initialHeap != "" {
		cxcore.INIT_HEAP_SIZE = parseMemoryString(options.initialHeap)
	}
	if options.maxHeap != "" {
		cxcore.MAX_HEAP_SIZE = parseMemoryString(options.maxHeap)
		if cxcore.MAX_HEAP_SIZE < cxcore.INIT_HEAP_SIZE {
			// Then MAX_HEAP_SIZE overrides INIT_HEAP_SIZE's value.
			cxcore.INIT_HEAP_SIZE = cxcore.MAX_HEAP_SIZE
		}
	}
	if options.stackSize != "" {
		cxcore.STACK_SIZE = parseMemoryString(options.stackSize)
		actions.DataOffset = cxcore.STACK_SIZE
	}
	if options.minHeapFreeRatio != float64(0) {
		cxcore.MIN_HEAP_FREE_RATIO = float32(options.minHeapFreeRatio)
	}
	if options.maxHeapFreeRatio != float64(0) {
		cxcore.MAX_HEAP_FREE_RATIO = float32(options.maxHeapFreeRatio)
	}

	// options, file pointers, filenames
	cxArgs, sourceCode, fileNames := cxcore.ParseArgsForCX(commandLine.Args(), true)

	// Propagate some options out to other packages.
	parser.DebugLexer = options.debugLexer // in package parser
	DebugProfileRate = options.debugProfile
	DebugProfile = DebugProfileRate > 0

	if run, bcHeap, sPrgrm := parseProgram(options, fileNames, sourceCode); run {
		runProgram(options, cxArgs, sourceCode, bcHeap, sPrgrm)
	}
}

// mergeBlockchainHeap adds the heap `bcHeap` found in the program state of a CX
// chain to the program to be run `PRGRM` and updates all the references to heap
// objects found in the transaction code considering the data segment found in
// the serialized program `sPrgrm`.
func mergeBlockchainHeap(bcHeap, sPrgrm []byte) {
	// Setting the CX runtime to run `PRGRM`.
	actions.PRGRM.SelectProgram()

	bcHeapLen := len(bcHeap)
	remHeapSpace := len(actions.PRGRM.Memory[actions.PRGRM.HeapStartsAt:])
	fullDataSegSize := actions.PRGRM.HeapStartsAt - actions.PRGRM.StackSize
	// Copying blockchain code heap.
	if bcHeapLen > remHeapSpace {
		// We don't have enough space. We're using the available bytes...
		for c := 0; c < remHeapSpace; c++ {
			actions.PRGRM.Memory[actions.PRGRM.HeapStartsAt+c] = bcHeap[c]
		}
		// ...and then we append the remaining bytes.
		actions.PRGRM.Memory = append(actions.PRGRM.Memory, bcHeap[remHeapSpace:]...)
	} else {
		// We have enough space and we simply write the bytes.
		for c := 0; c < bcHeapLen; c++ {
			actions.PRGRM.Memory[actions.PRGRM.HeapStartsAt+c] = bcHeap[c]
		}
	}
	// Recalculating the heap size.
	actions.PRGRM.HeapSize = len(actions.PRGRM.Memory) - actions.PRGRM.HeapStartsAt
	txnDataLen := fullDataSegSize - cxcore.GetSerializedDataSize(sPrgrm)
	// TODO: CX chains only work with one package at the moment (in the blockchain code). That is what that "1" is for.
	// Displacing the references to heap objects by `txnDataLen`.
	// This needs to be done as the addresses to the heap objects are displaced
	// by the addition of the transaction code's data segment.
	cxcore.DisplaceReferences(actions.PRGRM, txnDataLen, 1)
}

// Used for the -heap-initial, -heap-max and -stack-size flags.
// This function parses, for example, "1M" to 1048576 (the corresponding number of bytes)
// Possible suffixes are: G or g (gigabytes), M or m (megabytes), K or k (kilobytes)
func parseMemoryString(s string) int {
	suffix := s[len(s)-1]
	_, notSuffix := strconv.ParseFloat(string(suffix), 64)

	if notSuffix == nil {
		// then we don't have a suffix
		num, err := strconv.ParseInt(s, 10, 64)

		if err != nil {
			// malformed size
			return -1
		}

		return int(num)
	} else {
		// then we have a suffix
		num, err := strconv.ParseFloat(s[:len(s)-1], 64)

		if err != nil {
			// malformed size
			return -1
		}

		// The user can use suffixes to give as input gigabytes, megabytes or kilobytes.
		switch suffix {
		case 'G', 'g':
			return int(num * 1073741824)
		case 'M', 'm':
			return int(num * 1048576)
		case 'K', 'k':
			return int(num * 1024)
		default:
			return -1
		}
	}
}

func unsafeEval(code string) (out string) {
	var lexer *parser.Lexer
	defer func() {
		if r := recover(); r != nil {
			out = fmt.Sprintf("%v", r)
			lexer.Stop()
		}
	}()

	// storing strings sent to standard output
	old := os.Stdout
	r, w, _ := os.Pipe()
	os.Stdout = w

	actions.LineNo = 0

	actions.PRGRM = cxcore.MakeProgram()
	cxgo0.PRGRM0 = actions.PRGRM

	cxgo0.Parse(code)

	actions.PRGRM = cxgo0.PRGRM0

	lexer = parser.NewLexer(bytes.NewBufferString(code))
	parser.Parse(lexer)
	//yyParse(lexer)

	addInitFunction(actions.PRGRM)

	if err := actions.PRGRM.RunCompiled(0, nil); err != nil {
		actions.PRGRM = cxcore.MakeProgram()
		return fmt.Sprintf("%s", err)
	}

	outC := make(chan string)
	go func() {
		var buf bytes.Buffer
		io.Copy(&buf, r)
		outC <- buf.String()
	}()

	w.Close()
	os.Stdout = old // restoring the real stdout
	out = <-outC

	actions.PRGRM = cxcore.MakeProgram()
	return out
}

func Eval(code string) string {
	runtime.GOMAXPROCS(2)
	ch := make(chan string, 1)

	var result string

	go func() {
		result = unsafeEval(code)
		ch <- result
	}()

	timer := time.NewTimer(20 * time.Second)
	defer timer.Stop()

	select {
	case <-ch:
		return result
	case <-timer.C:
		actions.PRGRM = cxcore.MakeProgram()
		return "Timed out."
	}
}

type SourceCode struct {
	Code string
}

func ServiceMode() {
	host := ":5336"

	mux := http.NewServeMux()

	mux.Handle("/", http.FileServer(http.Dir("./dist")))
	mux.Handle("/program/", api2.NewAPI("/program", actions.PRGRM))
	mux.HandleFunc("/eval", func(w http.ResponseWriter, r *http.Request) {
		defer r.Body.Close()
		var b []byte
		var err error
		if b, err = ioutil.ReadAll(r.Body); err != nil {
			http.Error(w, err.Error(), 500)
			return
		}

		var source SourceCode
		if err := json.Unmarshal(b, &source); err != nil {
			http.Error(w, err.Error(), 500)
			return
		}

		if err := r.ParseForm(); err == nil {
			fmt.Fprintf(w, "%s", Eval(source.Code+"\n"))
		}
	})

	if listener, err := net.Listen("tcp", host); err == nil {
		fmt.Println("Starting CX web service on http://127.0.0.1:5336/")
		http.Serve(listener, mux)
	}
}

func IdeServiceMode() {
	// Leaps's host address
	ideHost := "localhost:5335"

	// Working directory for ide
	sharedPath := fmt.Sprintf("%s/src/github.com/SkycoinProject/cx", os.Getenv("GOPATH"))

	// Start Leaps
	// cmd = `leaps -address localhost:5335 $GOPATH/src/skycoin/cx`
	cmnd := exec.Command("leaps", "-address", ideHost, sharedPath)

	// Just leave start command
	cmnd.Start()
}

func PersistentServiceMode() {
	fmt.Println("Start persistent for service mode!")

	fi := bufio.NewReader(os.Stdin)

	for {
		var inp string
		var ok bool

		printPrompt()

		if inp, ok = readline(fi); ok {
			if isJSON(inp) {
				var err error
				client := &http.Client{}
				body := bytes.NewBufferString(inp)
				req, err := http.NewRequest("GET", "http://127.0.0.1:5336/eval", body)
				if err != nil {
					fmt.Println(err)
					return
				}

				if resp, err := client.Do(req); err != nil {
					fmt.Println(err)
				} else {
					fmt.Println(resp.Status)
				}
			}
		}
	}
}

func determineWorkDir(filename string) string {
	filename = filepath.FromSlash(filename)

	i := strings.LastIndexByte(filename, os.PathSeparator)
	if i == -1 {
		i = 0
	}
	return filename[:i]
}

func printPrompt() {
	if actions.ReplTargetMod != "" {
		fmt.Println(fmt.Sprintf(":package %s ...", actions.ReplTargetMod))
		fmt.Printf("* ")
	} else if actions.ReplTargetFn != "" {
		fmt.Println(fmt.Sprintf(":func %s {...", actions.ReplTargetFn))
		fmt.Printf("\t* ")
	} else if actions.ReplTargetStrct != "" {
		fmt.Println(fmt.Sprintf(":struct %s {...", actions.ReplTargetStrct))
		fmt.Printf("\t* ")
	} else {
		fmt.Printf("* ")
	}
}

func repl() {
	fmt.Println("CX", VERSION)
	fmt.Println("More information about CX is available at http://cx.skycoin.com/ and https://github.com/SkycoinProject/cx/")

	cxcore.InREPL = true

	// fi := bufio.NewReader(os.NewFile(0, "stdin"))
	fi := bufio.NewReader(os.Stdin)
	// scanner := bufio.NewScanner(os.Stdin)

	for {
		var inp string
		var ok bool

		printPrompt()

		if inp, ok = readline(fi); ok {
			if actions.ReplTargetFn != "" {
				inp = fmt.Sprintf(":func %s {\n%s\n}\n", actions.ReplTargetFn, inp)
			}
			if actions.ReplTargetMod != "" {
				inp = fmt.Sprintf("%s", inp)
			}
			if actions.ReplTargetStrct != "" {
				inp = fmt.Sprintf(":struct %s {\n%s\n}\n", actions.ReplTargetStrct, inp)
			}

			b := bytes.NewBufferString(inp)

			parser.Parse(parser.NewLexer(b))
			//yyParse(NewLexer(b))
		} else {
			if actions.ReplTargetFn != "" {
				actions.ReplTargetFn = ""
				continue
			}

			if actions.ReplTargetStrct != "" {
				actions.ReplTargetStrct = ""
				continue
			}

			if actions.ReplTargetMod != "" {
				actions.ReplTargetMod = ""
				continue
			}

			fmt.Printf("\nBye!\n")
			break
		}
	}
}

// chainStatePrelude initializes the program structure `prgrm` with data from
// the program state stored on a CX chain.
func chainStatePrelude(sPrgrm *[]byte, bcHeap *[]byte, prgrm *cxcore.CXProgram) {
	resp, err := http.Get("http://127.0.0.1:6420/api/v1/programState?addrs=TkyD4wD64UE6M5BkNQA17zaf7Xcg4AufwX")
	if err != nil {
		fmt.Println(err)
		return
	}
	defer resp.Body.Close()
	body, err := ioutil.ReadAll(resp.Body)

	if err := json.Unmarshal(body, &sPrgrm); err != nil {
		fmt.Println(string(body))
		return
	}

	memOff := cxcore.GetSerializedMemoryOffset(*sPrgrm)
	stackSize := cxcore.GetSerializedStackSize(*sPrgrm)
	// sPrgrm with Stack and Heap
	sPrgrmSH := (*sPrgrm)[:memOff]
	// Appending new stack
	sPrgrmSH = append(sPrgrmSH, make([]byte, stackSize)...)
	// Appending data and heap segment
	sPrgrmSH = append(sPrgrmSH, (*sPrgrm)[memOff:]...)
	*bcHeap = (*sPrgrm)[memOff+cxcore.GetSerializedDataSize(*sPrgrm):]

	*prgrm = *cxcore.Deserialize(sPrgrmSH)
	// We need to start adding new data elements after the CX chain
	// program state's data segment
	actions.DataOffset = prgrm.HeapStartsAt
}

// initMainPkg adds a `main` package with an empty `main` function to `prgrm`.
func initMainPkg(prgrm *cxcore.CXProgram) {
	mod := cxcore.MakePackage(cxcore.MAIN_PKG)
	prgrm.AddPackage(mod)
	fn := cxcore.MakeFunction(cxcore.MAIN_FUNC, actions.CurrentFile, actions.LineNo)
	mod.AddFunction(fn)
}

// checkCXPathSet checks if the user has set the environment variable
// `CXPATH`. If not, CX creates a workspace at $HOME/cx, along with $HOME/cx/bin,
// $HOME/cx/pkg and $HOME/cx/src
func checkCXPathSet(options cxCmdFlags) {
	// Determining the filepath of the directory where the user
	// started the `cx` command.
	_, err := os.Executable()
	if err != nil {
		panic(err)
	}
	// cxcore.COREPATH = filepath.Dir(ex) // TODO @evanlinjin: Not used.

	CXPATH := ""
	if os.Getenv("CXPATH") != "" {
		CXPATH = os.Getenv("CXPATH")
	}
	// `options.cxpath` overrides `os.Getenv("CXPATH")`
	if options.cxpath != "" {
		CXPATH, err = filepath.Abs(options.cxpath)
		if err != nil {
			panic(err)
		}
	}
	if os.Getenv("CXPATH") == "" && options.cxpath == "" {
		usr, err := user.Current()
		if err != nil {
			panic(err)
		}

		CXPATH = usr.HomeDir + "/cx/"
	}

	cxcore.BINPATH = filepath.Join(CXPATH, "bin/")
	cxcore.PKGPATH = filepath.Join(CXPATH, "pkg/")
	cxcore.SRCPATH = filepath.Join(CXPATH, "src/")

	// Creating directories in case they do not exist.
	if _, err := cxcore.CXStatFile(CXPATH); os.IsNotExist(err) {
		cxcore.CXMkdirAll(CXPATH, 0755)
	}
	if _, err := cxcore.CXStatFile(cxcore.BINPATH); os.IsNotExist(err) {
		cxcore.CXMkdirAll(cxcore.BINPATH, 0755)
	}
	if _, err := cxcore.CXStatFile(cxcore.PKGPATH); os.IsNotExist(err) {
		cxcore.CXMkdirAll(cxcore.PKGPATH, 0755)
	}
	if _, err := cxcore.CXStatFile(cxcore.SRCPATH); os.IsNotExist(err) {
		cxcore.CXMkdirAll(cxcore.SRCPATH, 0755)
	}
}

func addInitFunction(PRGRM *cxcore.CXProgram) {
	mainPkg, err := PRGRM.GetPackage(cxcore.MAIN_PKG)
	if err != nil {
		panic(err)
	}

	initFn := cxcore.MakeFunction(cxcore.SYS_INIT_FUNC, actions.CurrentFile, actions.LineNo)
	mainPkg.AddFunction(initFn)

	actions.FunctionDeclaration(initFn, nil, nil, actions.SysInitExprs)
	if _, err := PRGRM.SelectFunction(cxcore.MAIN_FUNC); err != nil {
		panic(err)
	}
}

// ----------------------------------------------------------------
//                     Utility functions

func readline(fi *bufio.Reader) (string, bool) {
	s, err := fi.ReadString('\n')

	s = strings.Replace(s, "\n", "", -1)
	s = strings.Replace(s, "\r", "", -1)

	for _, ch := range s {
		if ch == rune(4) {
			err = io.EOF
			break
		}
	}

	if err != nil {
		return "", false
	}

	return s, true
}

func isJSON(str string) bool {
	var js map[string]interface{}
	err := json.Unmarshal([]byte(str), &js)
	return err == nil
}
