package main

import (
	"bufio"
	"encoding/hex"
	"fmt"
	"io/ioutil"
	"net"
	"net/http"
	"net/url"
	"os"
	"path"
	"strconv"
	"strings"
	"time"

	"golang.org/x/crypto/ssh/terminal"

	"encoding/json"
	"github.com/Sirupsen/logrus"
	"github.com/docker/distribution/registry/client/auth"
	"github.com/docker/distribution/registry/client/auth/challenge"
	"github.com/docker/distribution/registry/client/transport"
	"github.com/docker/go-connections/tlsconfig"
	"github.com/docker/notary"
	notaryclient "github.com/docker/notary/client"
	"github.com/docker/notary/cryptoservice"
	"github.com/docker/notary/passphrase"
	"github.com/docker/notary/trustmanager"
	"github.com/docker/notary/trustpinning"
	"github.com/docker/notary/tuf/data"
	tufutils "github.com/docker/notary/tuf/utils"
	"github.com/docker/notary/utils"
	"github.com/spf13/cobra"
	"github.com/spf13/viper"
	"path/filepath"
)

var cmdTUFListTemplate = usageTemplate{
	Use:   "list [ GUN ]",
	Short: "Lists targets for a remote trusted collection.",
	Long:  "Lists all targets for a remote trusted collection identified by the Globally Unique Name. This is an online operation.",
}

var cmdTUFAddTemplate = usageTemplate{
	Use:   "add [ GUN ] <target> <file>",
	Short: "Adds the file as a target to the trusted collection.",
	Long:  "Adds the file as a target to the local trusted collection identified by the Globally Unique Name. This is an offline operation.  Please then use `publish` to push the changes to the remote trusted collection.",
}

var cmdTUFAddHashTemplate = usageTemplate{
	Use:   "addhash [ GUN ] <target> <byte size> <hashes>",
	Short: "Adds the byte size and hash(es) as a target to the trusted collection.",
	Long:  "Adds the specified byte size and hash(es) as a target to the local trusted collection identified by the Globally Unique Name. This is an offline operation.  Please then use `publish` to push the changes to the remote trusted collection.",
}

var cmdTUFRemoveTemplate = usageTemplate{
	Use:   "remove [ GUN ] <target>",
	Short: "Removes a target from a trusted collection.",
	Long:  "Removes a target from the local trusted collection identified by the Globally Unique Name. This is an offline operation.  Please then use `publish` to push the changes to the remote trusted collection.",
}

var cmdTUFInitTemplate = usageTemplate{
	Use:   "init [ GUN ]",
	Short: "Initializes a local trusted collection.",
	Long:  "Initializes a local trusted collection identified by the Globally Unique Name. This is an online operation.",
}

var cmdTUFLookupTemplate = usageTemplate{
	Use:   "lookup [ GUN ] <target>",
	Short: "Looks up a specific target in a remote trusted collection.",
	Long:  "Looks up a specific target in a remote trusted collection identified by the Globally Unique Name.",
}

var cmdTUFPublishTemplate = usageTemplate{
	Use:   "publish [ GUN ]",
	Short: "Publishes the local trusted collection.",
	Long:  "Publishes the local trusted collection identified by the Globally Unique Name, sending the local changes to a remote trusted server.",
}

var cmdTUFStatusTemplate = usageTemplate{
	Use:   "status [ GUN ]",
	Short: "Displays status of unpublished changes to the local trusted collection.",
	Long:  "Displays status of unpublished changes to the local trusted collection identified by the Globally Unique Name.",
}

var cmdTUFResetTemplate = usageTemplate{
	Use:   "reset [ GUN ]",
	Short: "Resets unpublished changes for the local trusted collection.",
	Long:  "Resets unpublished changes for the local trusted collection identified by the Globally Unique Name.",
}

var cmdTUFVerifyTemplate = usageTemplate{
	Use:   "verify [ GUN ] <target>",
	Short: "Verifies if the content is included in the remote trusted collection",
	Long:  "Verifies if the data passed in STDIN is included in the remote trusted collection identified by the Globally Unique Name.",
}

var cmdWitnessTemplate = usageTemplate{
	Use:   "witness [ GUN ] <role> ...",
	Short: "Marks roles to be re-signed the next time they're published",
	Long:  "Marks roles to be re-signed the next time they're published. Currently will always bump version and expiry for role. N.B. behaviour may change when thresholding is introduced.",
}

var cmdTUFDeleteTemplate = usageTemplate{
	Use:   "delete [ GUN ]",
	Short: "Deletes all content for a trusted collection",
	Long:  "Deletes all local content for a trusted collection identified by the Globally Unique Name. Remote data can also be deleted with an additional flag.",
}

var cmdTUFExportTemplate = usageTemplate{
	Use:   "export <GUN> [ roles ... ]",
	Short: "Export the listed role files for a trusted collection.",
	Long:  "Export the listed role files for a trusted collection identified by the Globally Unique Name in the appropriate TUF file structure.",
}

type tufCommander struct {
	// these need to be set
	configGetter func() (*viper.Viper, error)
	retriever    notary.PassRetriever

	// these are for command line parsing - no need to set
	roles   []string
	sha256  string
	sha512  string
	rootKey string

	input  string
	output string
	quiet  bool

	resetAll          bool
	deleteIdx         []int
	archiveChangelist string

	deleteRemote bool

	autoPublish bool
}

func (t *tufCommander) AddToCommand(cmd *cobra.Command) {
	cmdTUFInit := cmdTUFInitTemplate.ToCommand(t.tufInit)
	cmdTUFInit.Flags().StringVar(&t.rootKey, "rootkey", "", "Root key to initialize the repository with")
	cmdTUFInit.Flags().BoolVarP(&t.autoPublish, "publish", "p", false, htAutoPublish)
	cmd.AddCommand(cmdTUFInit)

	cmd.AddCommand(cmdTUFStatusTemplate.ToCommand(t.tufStatus))

	cmdReset := cmdTUFResetTemplate.ToCommand(t.tufReset)
	cmdReset.Flags().IntSliceVarP(&t.deleteIdx, "number", "n", nil, "Numbers of specific changes to exclusively reset, as shown in status list")
	cmdReset.Flags().BoolVar(&t.resetAll, "all", false, "Reset all changes shown in the status list")
	cmd.AddCommand(cmdReset)

	cmd.AddCommand(cmdTUFPublishTemplate.ToCommand(t.tufPublish))

	cmd.AddCommand(cmdTUFLookupTemplate.ToCommand(t.tufLookup))

	cmdTUFList := cmdTUFListTemplate.ToCommand(t.tufList)
	cmdTUFList.Flags().StringSliceVarP(
		&t.roles, "roles", "r", nil, "Delegation roles to list targets for (will shadow targets role)")
	cmd.AddCommand(cmdTUFList)

	cmdTUFAdd := cmdTUFAddTemplate.ToCommand(t.tufAdd)
	cmdTUFAdd.Flags().StringSliceVarP(&t.roles, "roles", "r", nil, "Delegation roles to add this target to")
	cmdTUFAdd.Flags().BoolVarP(&t.autoPublish, "publish", "p", false, htAutoPublish)
	cmd.AddCommand(cmdTUFAdd)

	cmdTUFRemove := cmdTUFRemoveTemplate.ToCommand(t.tufRemove)
	cmdTUFRemove.Flags().StringSliceVarP(&t.roles, "roles", "r", nil, "Delegation roles to remove this target from")
	cmdTUFRemove.Flags().BoolVarP(&t.autoPublish, "publish", "p", false, htAutoPublish)
	cmd.AddCommand(cmdTUFRemove)

	cmdTUFAddHash := cmdTUFAddHashTemplate.ToCommand(t.tufAddByHash)
	cmdTUFAddHash.Flags().StringSliceVarP(&t.roles, "roles", "r", nil, "Delegation roles to add this target to")
	cmdTUFAddHash.Flags().StringVar(&t.sha256, notary.SHA256, "", "hex encoded sha256 of the target to add")
	cmdTUFAddHash.Flags().StringVar(&t.sha512, notary.SHA512, "", "hex encoded sha512 of the target to add")
	cmdTUFAddHash.Flags().BoolVarP(&t.autoPublish, "publish", "p", false, htAutoPublish)
	cmd.AddCommand(cmdTUFAddHash)

	cmdTUFVerify := cmdTUFVerifyTemplate.ToCommand(t.tufVerify)
	cmdTUFVerify.Flags().StringVarP(&t.input, "input", "i", "", "Read from a file, instead of STDIN")
	cmdTUFVerify.Flags().StringVarP(&t.output, "output", "o", "", "Write to a file, instead of STDOUT")
	cmdTUFVerify.Flags().BoolVarP(&t.quiet, "quiet", "q", false, "No output except for errors")
	cmd.AddCommand(cmdTUFVerify)

	cmdWitness := cmdWitnessTemplate.ToCommand(t.tufWitness)
	cmdWitness.Flags().BoolVarP(&t.autoPublish, "publish", "p", false, htAutoPublish)
	cmd.AddCommand(cmdWitness)

	cmdTUFDeleteGUN := cmdTUFDeleteTemplate.ToCommand(t.tufDeleteGUN)
	cmdTUFDeleteGUN.Flags().BoolVar(&t.deleteRemote, "remote", false, "Delete remote data for GUN in addition to local cache")
	cmd.AddCommand(cmdTUFDeleteGUN)

	cmdTUFExportGUN := cmdTUFExportTemplate.ToCommand(t.tufExportGUN)
	cmdTUFExportGUN.Flags().StringVarP(&t.output, "output", "o", "", "File to export role files to")
	cmd.AddCommand(cmdTUFExportGUN)
}

func (t *tufCommander) tufExportGUN(cmd *cobra.Command, args []string) error {
	if len(args) < 1 {
		cmd.Usage()
		return fmt.Errorf("Please provide a GUN")
	}

	config, err := t.configGetter()
	if err != nil {
		return err
	}

	gun := data.GUN(args[0])

	roles := data.NewRoleList(args[1:])

	outDir := t.output
	if outDir == "" {
		currDir, err := os.Getwd()
		if err != nil {
			return err
		}

		outDir = path.Join(currDir, filepath.FromSlash(gun.String()))
	}

	rt, err := getTransport(config, gun, readOnly)
	if err != nil {
		return err
	}

	trustPin, err := getTrustPinning(config)
	if err != nil {
		return err
	}

	nRepo, err := notaryclient.NewFileCachedNotaryRepository(
		config.GetString("trust_dir"), gun, getRemoteTrustServer(config), rt, t.retriever, trustPin)
	if err != nil {
		return err
	}

	// if no role is provided, we export all targets for each registered role
	if len(roles) == 0 {
		rolesWithSig, err := nRepo.ListRoles()
		if err != nil {
			return err
		}

		roles = make([]data.RoleName, len(rolesWithSig))
		for index, roleWithSig := range rolesWithSig {
			roles[index] = roleWithSig.Name
		}
	}

	// a map of roles to target names and their associated json files
	exportFiles := make(map[data.RoleName]map[string][]byte)

	for _, role := range roles {
		targets, err := nRepo.ListTargets(role)
		if err != nil {
			return err
		}

		for _, target := range targets {
			metadata, err := nRepo.GetAllTargetMetadataByName(target.Name)
			if err != nil {
				return err
			}

			metadataJson, err := json.Marshal(metadata)
			if err != nil {
				return err
			}

			if _, ok := exportFiles[role]; !ok {
				exportFiles[role] = make(map[string][]byte)

			}

			exportFiles[role][target.Name] = metadataJson
		}
	}

	if len(exportFiles) == 0 {
		return nil
	}

	return exportGUNFiles(outDir, exportFiles, roles...)
}

func exportGUNFiles(outDir string, roleNameToTargetFiles map[data.RoleName]map[string][]byte, roles ...data.RoleName) error {
	err := os.MkdirAll(outDir, notary.PrivExecPerms)
	if err != nil {
		return nil
	}

	for role, targetNamesToFiles := range roleNameToTargetFiles {
		targetPath := strings.Join([]string{"0", role.String(), "json"}, ".")
		outFile := path.Join(outDir, targetPath)

		// Make sure the parent directory of the file is created (ex: "0.targets/" for "0.targets/foo.json")
		err := os.MkdirAll(filepath.Dir(outFile), notary.PrivExecPerms)
		if err != nil {
			return nil
		}

		if _, err := os.Stat(outFile); os.IsExist(err) {
			logrus.Warnf("Trying to export files for role %s but file %s exists, overwriting.", role.String(), outFile)
		}

		content, err := json.Marshal(targetNamesToFiles)
		if err != nil {
			return err
		}

		logrus.Debugf("Exporting targets for role %s in file %s.", role, outFile)
		if err := ioutil.WriteFile(outFile, content, notary.PrivNoExecPerms); err != nil {
			return err
		}
	}

	return nil
}

func (t *tufCommander) tufWitness(cmd *cobra.Command, args []string) error {
	if len(args) < 2 {
		cmd.Usage()
		return fmt.Errorf("Please provide a GUN and at least one role to witness")
	}
	config, err := t.configGetter()
	if err != nil {
		return err
	}

	gun := data.GUN(args[0])
	roles := data.NewRoleList(args[1:])

	fact := ConfigureRepo(config, t.retriever, false)
	nRepo, err := fact(gun)
	if err != nil {
		return err
	}

	success, err := nRepo.Witness(roles...)
	if err != nil {
		cmd.Printf("Some roles have failed to be marked for witnessing: %s", err.Error())
	}

	cmd.Printf(
		"The following roles were successfully marked for witnessing on the next publish:\n\t- %s\n",
		strings.Join(data.RolesListToStringList(success), "\n\t- "),
	)

	return maybeAutoPublish(cmd, t.autoPublish, gun, config, t.retriever)
}

func getTargetHashes(t *tufCommander) (data.Hashes, error) {
	targetHash := data.Hashes{}

	if t.sha256 != "" {
		if len(t.sha256) != notary.SHA256HexSize {
			return nil, fmt.Errorf("invalid sha256 hex contents provided")
		}
		sha256Hash, err := hex.DecodeString(t.sha256)
		if err != nil {
			return nil, err
		}
		targetHash[notary.SHA256] = sha256Hash
	}

	if t.sha512 != "" {
		if len(t.sha512) != notary.SHA512HexSize {
			return nil, fmt.Errorf("invalid sha512 hex contents provided")
		}
		sha512Hash, err := hex.DecodeString(t.sha512)
		if err != nil {
			return nil, err
		}
		targetHash[notary.SHA512] = sha512Hash
	}

	return targetHash, nil
}

func (t *tufCommander) tufAddByHash(cmd *cobra.Command, args []string) error {
	if len(args) < 3 || t.sha256 == "" && t.sha512 == "" {
		cmd.Usage()
		return fmt.Errorf("Must specify a GUN, target, byte size of target data, and at least one hash")
	}
	config, err := t.configGetter()
	if err != nil {
		return err
	}

	gun := data.GUN(args[0])
	targetName := args[1]
	targetSize := args[2]

	targetInt64Len, err := strconv.ParseInt(targetSize, 0, 64)
	if err != nil {
		return err
	}

	fact := ConfigureRepo(config, t.retriever, false)
	nRepo, err := fact(gun)
	if err != nil {
		return err
	}

	targetHashes, err := getTargetHashes(t)
	if err != nil {
		return err
	}

	// Manually construct the target with the given byte size and hashes
	target := &notaryclient.Target{Name: targetName, Hashes: targetHashes, Length: targetInt64Len}

	roleNames := data.NewRoleList(t.roles)

	// If roles is empty, we default to adding to targets
	if err = nRepo.AddTarget(target, roleNames...); err != nil {
		return err
	}

	// Include the hash algorithms we're using for pretty printing
	hashesUsed := []string{}
	for hashName := range targetHashes {
		hashesUsed = append(hashesUsed, hashName)
	}
	cmd.Printf(
		"Addition of target \"%s\" by %s hash to repository \"%s\" staged for next publish.\n",
		targetName, strings.Join(hashesUsed, ", "), gun)

	return maybeAutoPublish(cmd, t.autoPublish, gun, config, t.retriever)
}

func (t *tufCommander) tufAdd(cmd *cobra.Command, args []string) error {
	if len(args) < 3 {
		cmd.Usage()
		return fmt.Errorf("Must specify a GUN, target, and path to target data")
	}
	config, err := t.configGetter()
	if err != nil {
		return err
	}

	gun := data.GUN(args[0])
	targetName := args[1]
	targetPath := args[2]

	fact := ConfigureRepo(config, t.retriever, false)
	nRepo, err := fact(gun)
	if err != nil {
		return err
	}

	target, err := notaryclient.NewTarget(targetName, targetPath)
	if err != nil {
		return err
	}
	// If roles is empty, we default to adding to targets
	if err = nRepo.AddTarget(target, data.NewRoleList(t.roles)...); err != nil {
		return err
	}

	cmd.Printf("Addition of target \"%s\" to repository \"%s\" staged for next publish.\n", targetName, gun)

	return maybeAutoPublish(cmd, t.autoPublish, gun, config, t.retriever)
}

func (t *tufCommander) tufDeleteGUN(cmd *cobra.Command, args []string) error {
	if len(args) < 1 {
		cmd.Usage()
		return fmt.Errorf("Must specify a GUN")
	}
	config, err := t.configGetter()
	if err != nil {
		return err
	}

	gun := data.GUN(args[0])

	// Only initialize a roundtripper if we get the remote flag
	var remoteDeleteInfo string
	if t.deleteRemote {
		remoteDeleteInfo = " and remote"
	}

	rt, err := getTransport(config, gun, admin)
	if err != nil {
		return err
	}

	cmd.Printf("Deleting trust data for repository %s\n", gun)

	if err := notaryclient.DeleteTrustData(
		config.GetString("trust_dir"),
		gun,
		getRemoteTrustServer(config),
		rt,
		t.deleteRemote,
	); err != nil {
		return err
	}
	cmd.Printf("Successfully deleted local%s trust data for repository %s\n", remoteDeleteInfo, gun)
	return nil
}

func importRootKey(cmd *cobra.Command, rootKey string, nRepo notaryclient.Repository, retriever notary.PassRetriever) ([]string, error) {
	var rootKeyList []string

	if rootKey != "" {
		privKey, err := readKey(data.CanonicalRootRole, rootKey, retriever)
		if err != nil {
			return nil, err
		}
		err = nRepo.CryptoService().AddKey(data.CanonicalRootRole, "", privKey)
		if err != nil {
			return nil, fmt.Errorf("Error importing key: %v", err)
		}
		rootKeyList = []string{privKey.ID()}
	} else {
		rootKeyList = nRepo.CryptoService().ListKeys(data.CanonicalRootRole)
	}

	if len(rootKeyList) > 0 {
		// Chooses the first root key available, which is initialization specific
		// but should return the HW one first.
		rootKeyID := rootKeyList[0]
		cmd.Printf("Root key found, using: %s\n", rootKeyID)

		return []string{rootKeyID}, nil
	}

	return []string{}, nil
}

func (t *tufCommander) tufInit(cmd *cobra.Command, args []string) error {
	if len(args) < 1 {
		cmd.Usage()
		return fmt.Errorf("Must specify a GUN")
	}

	config, err := t.configGetter()
	if err != nil {
		return err
	}
	gun := data.GUN(args[0])

	fact := ConfigureRepo(config, t.retriever, true)
	nRepo, err := fact(gun)
	if err != nil {
		return err
	}

	rootKeyIDs, err := importRootKey(cmd, t.rootKey, nRepo, t.retriever)
	if err != nil {
		return err
	}

	if err = nRepo.Initialize(rootKeyIDs); err != nil {
		return err
	}

	return maybeAutoPublish(cmd, t.autoPublish, gun, config, t.retriever)
}

// Attempt to read a role key from a file, and return it as a data.PrivateKey
// If key is for the Root role, it must be encrypted
func readKey(role data.RoleName, keyFilename string, retriever notary.PassRetriever) (data.PrivateKey, error) {
	keyFile, err := os.Open(keyFilename)
	if err != nil {
		return nil, fmt.Errorf("Opening file to import as a root key: %v", err)
	}
	defer keyFile.Close()

	pemBytes, err := ioutil.ReadAll(keyFile)
	if err != nil {
		return nil, fmt.Errorf("Error reading input root key file: %v", err)
	}
	isEncrypted := true
	if err = cryptoservice.CheckRootKeyIsEncrypted(pemBytes); err != nil {
		if role == data.CanonicalRootRole {
			return nil, err
		}
		isEncrypted = false
	}
	var privKey data.PrivateKey
	if isEncrypted {
		privKey, _, err = trustmanager.GetPasswdDecryptBytes(retriever, pemBytes, "", data.CanonicalRootRole.String())
	} else {
		privKey, err = tufutils.ParsePEMPrivateKey(pemBytes, "")
	}
	if err != nil {
		return nil, err
	}

	return privKey, nil
}

func (t *tufCommander) tufList(cmd *cobra.Command, args []string) error {
	if len(args) < 1 {
		cmd.Usage()
		return fmt.Errorf("Must specify a GUN")
	}
	config, err := t.configGetter()
	if err != nil {
		return err
	}
	gun := data.GUN(args[0])

	fact := ConfigureRepo(config, t.retriever, true)
	nRepo, err := fact(gun)
	if err != nil {
		return err
	}

	// Retrieve the remote list of signed targets, prioritizing the passed-in list over targets
	targetList, err := nRepo.ListTargets(data.NewRoleList(t.roles)...)
	if err != nil {
		return err
	}

	prettyPrintTargets(targetList, cmd.Out())
	return nil
}

func (t *tufCommander) tufLookup(cmd *cobra.Command, args []string) error {
	if len(args) < 2 {
		cmd.Usage()
		return fmt.Errorf("Must specify a GUN and target")
	}
	config, err := t.configGetter()
	if err != nil {
		return err
	}

	gun := data.GUN(args[0])
	targetName := args[1]

	fact := ConfigureRepo(config, t.retriever, true)
	nRepo, err := fact(gun)
	if err != nil {
		return err
	}

	target, err := nRepo.GetTargetByName(targetName)
	if err != nil {
		return err
	}

	cmd.Println(target.Name, fmt.Sprintf("sha256:%x", target.Hashes["sha256"]), target.Length)
	return nil
}

func (t *tufCommander) tufStatus(cmd *cobra.Command, args []string) error {
	if len(args) < 1 {
		cmd.Usage()
		return fmt.Errorf("Must specify a GUN")
	}

	config, err := t.configGetter()
	if err != nil {
		return err
	}
	gun := data.GUN(args[0])

	fact := ConfigureRepo(config, t.retriever, false)
	nRepo, err := fact(gun)
	if err != nil {
		return err
	}

	cl, err := nRepo.GetChangelist()
	if err != nil {
		return err
	}

	if len(cl.List()) == 0 {
		cmd.Printf("No unpublished changes for %s\n", gun)
		return nil
	}

	cmd.Printf("Unpublished changes for %s:\n\n", gun)
	tw := initTabWriter(
		[]string{"#", "ACTION", "SCOPE", "TYPE", "PATH"},
		cmd.Out(),
	)
	for i, ch := range cl.List() {
		fmt.Fprintf(
			tw,
			fiveItemRow,
			fmt.Sprintf("%d", i),
			ch.Action(),
			ch.Scope(),
			ch.Type(),
			ch.Path(),
		)
	}
	tw.Flush()
	return nil
}

func (t *tufCommander) tufReset(cmd *cobra.Command, args []string) error {
	if len(args) < 1 {
		cmd.Usage()
		return fmt.Errorf("Must specify a GUN")
	}
	if !t.resetAll && len(t.deleteIdx) < 1 {
		cmd.Usage()
		return fmt.Errorf("Must specify changes to reset with -n or the --all flag")
	}

	config, err := t.configGetter()
	if err != nil {
		return err
	}
	gun := data.GUN(args[0])

	fact := ConfigureRepo(config, t.retriever, false)
	nRepo, err := fact(gun)
	if err != nil {
		return err
	}

	cl, err := nRepo.GetChangelist()
	if err != nil {
		return err
	}

	if t.resetAll {
		err = cl.Clear(t.archiveChangelist)
	} else {
		err = cl.Remove(t.deleteIdx)
	}
	// If it was a success, print to terminal
	if err == nil {
		cmd.Printf("Successfully reset specified changes for repository %s\n", gun)
	}
	return err
}

func (t *tufCommander) tufPublish(cmd *cobra.Command, args []string) error {
	if len(args) < 1 {
		cmd.Usage()
		return fmt.Errorf("Must specify a GUN")
	}

	config, err := t.configGetter()
	if err != nil {
		return err
	}
	gun := data.GUN(args[0])

	cmd.Println("Pushing changes to", gun)

	fact := ConfigureRepo(config, t.retriever, true)
	nRepo, err := fact(gun)
	if err != nil {
		return err
	}

	return publishAndPrintToCLI(cmd, nRepo)
}

func (t *tufCommander) tufRemove(cmd *cobra.Command, args []string) error {
	if len(args) < 2 {
		return fmt.Errorf("Must specify a GUN and target")
	}
	config, err := t.configGetter()
	if err != nil {
		return err
	}

	gun := data.GUN(args[0])
	targetName := args[1]

	fact := ConfigureRepo(config, t.retriever, false)
	repo, err := fact(gun)
	if err != nil {
		return err
	}

	// If roles is empty, we default to removing from targets
	if err = repo.RemoveTarget(targetName, data.NewRoleList(t.roles)...); err != nil {
		return err
	}

	cmd.Printf("Removal of %s from %s staged for next publish.\n", targetName, gun)

	return maybeAutoPublish(cmd, t.autoPublish, gun, config, t.retriever)
}

func (t *tufCommander) tufVerify(cmd *cobra.Command, args []string) error {
	if len(args) < 2 {
		cmd.Usage()
		return fmt.Errorf("Must specify a GUN and target")
	}

	config, err := t.configGetter()
	if err != nil {
		return err
	}

	payload, err := getPayload(t)
	if err != nil {
		return err
	}

	gun := data.GUN(args[0])
	targetName := args[1]

	fact := ConfigureRepo(config, t.retriever, true)
	nRepo, err := fact(gun)
	if err != nil {
		return err
	}

	target, err := nRepo.GetTargetByName(targetName)
	if err != nil {
		return fmt.Errorf("error retrieving target by name:%s, error:%v", targetName, err)
	}

	if err := data.CheckHashes(payload, targetName, target.Hashes); err != nil {
		return fmt.Errorf("data not present in the trusted collection, %v", err)
	}

	return feedback(t, payload)
}

type passwordStore struct {
	anonymous bool
}

func (ps passwordStore) Basic(u *url.URL) (string, string) {
	// if it's not a terminal, don't wait on input
	if ps.anonymous || !terminal.IsTerminal(int(os.Stdin.Fd())) {
		return "", ""
	}

	stdin := bufio.NewReader(os.Stdin)
	fmt.Fprintf(os.Stdout, "Enter username: ")

	userIn, err := stdin.ReadBytes('\n')
	if err != nil {
		logrus.Errorf("error processing username input: %s", err)
		return "", ""
	}

	username := strings.TrimSpace(string(userIn))

	fmt.Fprintf(os.Stdout, "Enter password: ")
	passphrase, err := passphrase.GetPassphrase(stdin)
	fmt.Fprintln(os.Stdout)
	if err != nil {
		logrus.Errorf("error processing password input: %s", err)
		return "", ""
	}
	password := strings.TrimSpace(string(passphrase))

	return username, password
}

// to comply with the CredentialStore interface
func (ps passwordStore) RefreshToken(u *url.URL, service string) string {
	return ""
}

// to comply with the CredentialStore interface
func (ps passwordStore) SetRefreshToken(u *url.URL, service string, token string) {
}

type httpAccess int

const (
	readOnly httpAccess = iota
	readWrite
	admin
)

// It correctly handles the auth challenge/credentials required to interact
// with a notary server over both HTTP Basic Auth and the JWT auth implemented
// in the notary-server
// The readOnly flag indicates if the operation should be performed as an
// anonymous read only operation. If the command entered requires write
// permissions on the server, readOnly must be false
func getTransport(config *viper.Viper, gun data.GUN, permission httpAccess) (http.RoundTripper, error) {
	// Attempt to get a root CA from the config file. Nil is the host defaults.
	rootCAFile := utils.GetPathRelativeToConfig(config, "remote_server.root_ca")
	clientCert := utils.GetPathRelativeToConfig(config, "remote_server.tls_client_cert")
	clientKey := utils.GetPathRelativeToConfig(config, "remote_server.tls_client_key")

	insecureSkipVerify := false
	if config.IsSet("remote_server.skipTLSVerify") {
		insecureSkipVerify = config.GetBool("remote_server.skipTLSVerify")
	}

	if clientCert == "" && clientKey != "" || clientCert != "" && clientKey == "" {
		return nil, fmt.Errorf("either pass both client key and cert, or neither")
	}

	tlsConfig, err := tlsconfig.Client(tlsconfig.Options{
		CAFile:             rootCAFile,
		InsecureSkipVerify: insecureSkipVerify,
		CertFile:           clientCert,
		KeyFile:            clientKey,
	})
	if err != nil {
		return nil, fmt.Errorf("unable to configure TLS: %s", err.Error())
	}

	base := &http.Transport{
		Proxy: http.ProxyFromEnvironment,
		Dial: (&net.Dialer{
			Timeout:   30 * time.Second,
			KeepAlive: 30 * time.Second,
			DualStack: true,
		}).Dial,
		TLSHandshakeTimeout: 10 * time.Second,
		TLSClientConfig:     tlsConfig,
		DisableKeepAlives:   true,
	}
	trustServerURL := getRemoteTrustServer(config)
	return tokenAuth(trustServerURL, base, gun, permission)
}

func tokenAuth(trustServerURL string, baseTransport *http.Transport, gun data.GUN,
	permission httpAccess) (http.RoundTripper, error) {

	// TODO(dmcgowan): add notary specific headers
	authTransport := transport.NewTransport(baseTransport)
	pingClient := &http.Client{
		Transport: authTransport,
		Timeout:   5 * time.Second,
	}
	endpoint, err := url.Parse(trustServerURL)
	if err != nil {
		return nil, fmt.Errorf("Could not parse remote trust server url (%s): %s", trustServerURL, err.Error())
	}
	if endpoint.Scheme == "" {
		return nil, fmt.Errorf("Trust server url has to be in the form of http(s)://URL:PORT. Got: %s", trustServerURL)
	}
	subPath, err := url.Parse(path.Join(endpoint.Path, "/v2") + "/")
	if err != nil {
		return nil, fmt.Errorf("Failed to parse v2 subpath. This error should not have been reached. Please report it as an issue at https://github.com/docker/notary/issues: %s", err.Error())
	}
	endpoint = endpoint.ResolveReference(subPath)
	req, err := http.NewRequest("GET", endpoint.String(), nil)
	if err != nil {
		return nil, err
	}
	resp, err := pingClient.Do(req)
	if err != nil {
		logrus.Errorf("could not reach %s: %s", trustServerURL, err.Error())
		logrus.Info("continuing in offline mode")
		return nil, nil
	}
	// non-nil err means we must close body
	defer resp.Body.Close()
	if (resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices) &&
		resp.StatusCode != http.StatusUnauthorized {
		// If we didn't get a 2XX range or 401 status code, we're not talking to a notary server.
		// The http client should be configured to handle redirects so at this point, 3XX is
		// not a valid status code.
		logrus.Errorf("could not reach %s: %d", trustServerURL, resp.StatusCode)
		logrus.Info("continuing in offline mode")
		return nil, nil
	}

	challengeManager := challenge.NewSimpleManager()
	if err := challengeManager.AddResponse(resp); err != nil {
		return nil, err
	}

	ps := passwordStore{anonymous: permission == readOnly}

	var actions []string
	switch permission {
	case admin:
		actions = []string{"*"}
	case readWrite:
		actions = []string{"push", "pull"}
	case readOnly:
		actions = []string{"pull"}
	default:
		return nil, fmt.Errorf("Invalid permission requested for token authentication of gun %s", gun)
	}

	tokenHandler := auth.NewTokenHandler(authTransport, ps, gun.String(), actions...)
	basicHandler := auth.NewBasicHandler(ps)

	modifier := auth.NewAuthorizer(challengeManager, tokenHandler, basicHandler)

	if permission != readOnly {
		return newAuthRoundTripper(transport.NewTransport(baseTransport, modifier)), nil
	}

	// Try to authenticate read only repositories using basic username/password authentication
	return newAuthRoundTripper(transport.NewTransport(baseTransport, modifier),
		transport.NewTransport(baseTransport, auth.NewAuthorizer(challengeManager, auth.NewTokenHandler(authTransport, passwordStore{anonymous: false}, gun.String(), actions...)))), nil
}

func getRemoteTrustServer(config *viper.Viper) string {
	if configRemote := config.GetString("remote_server.url"); configRemote != "" {
		return configRemote
	}
	return defaultServerURL
}

func getTrustPinning(config *viper.Viper) (trustpinning.TrustPinConfig, error) {
	var ok bool
	// Need to parse out Certs section from config
	certMap := config.GetStringMap("trust_pinning.certs")
	resultCertMap := make(map[string][]string)
	for gun, certSlice := range certMap {
		var castedCertSlice []interface{}
		if castedCertSlice, ok = certSlice.([]interface{}); !ok {
			return trustpinning.TrustPinConfig{}, fmt.Errorf("invalid format for trust_pinning.certs")
		}
		certsForGun := make([]string, len(castedCertSlice))
		for idx, certIDInterface := range castedCertSlice {
			if certID, ok := certIDInterface.(string); ok {
				certsForGun[idx] = certID
			} else {
				return trustpinning.TrustPinConfig{}, fmt.Errorf("invalid format for trust_pinning.certs")
			}
		}
		resultCertMap[gun] = certsForGun
	}
	return trustpinning.TrustPinConfig{
		DisableTOFU: config.GetBool("trust_pinning.disable_tofu"),
		CA:          config.GetStringMapString("trust_pinning.ca"),
		Certs:       resultCertMap,
	}, nil
}

// authRoundTripper tries to authenticate the requests via multiple HTTP transactions (until first succeed)
type authRoundTripper struct {
	trippers []http.RoundTripper
}

func newAuthRoundTripper(trippers ...http.RoundTripper) http.RoundTripper {
	return &authRoundTripper{trippers: trippers}
}

func (a *authRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {

	var resp *http.Response
	// Try all run all transactions
	for _, t := range a.trippers {
		var err error
		resp, err = t.RoundTrip(req)
		// Reject on error
		if err != nil {
			return resp, err
		}

		// Stop when request is authorized/unknown error
		if resp.StatusCode != http.StatusUnauthorized {
			return resp, nil
		}
	}

	// Return the last response
	return resp, nil
}

func maybeAutoPublish(cmd *cobra.Command, doPublish bool, gun data.GUN, config *viper.Viper, passRetriever notary.PassRetriever) error {

	if !doPublish {
		return nil
	}

	fact := ConfigureRepo(config, passRetriever, true)
	nRepo, err := fact(gun)
	if err != nil {
		return err
	}

	cmd.Println("Auto-publishing changes to", nRepo.GetGUN())
	return publishAndPrintToCLI(cmd, nRepo)
}

func publishAndPrintToCLI(cmd *cobra.Command, nRepo notaryclient.Repository) error {
	if err := nRepo.Publish(); err != nil {
		return err
	}
	cmd.Printf("Successfully published changes for repository %s\n", nRepo.GetGUN())
	return nil
}
