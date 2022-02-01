package main

import (
	"encoding/json"
	"log"
	"os"

	kube "github.com/duglin/k8sapi/lib"
)

// Labels we'll use.
// PAUSE_CHECK is used as an indicator that we've already processed this
// buildrun so we can skip it
// PAUSE_BUILD is used as a pointer to a buildrun from all knServices that
// are waiting for the buildrun to finish. It should contain the UID
// of the buildrun of interest.
var PAUSE_CHECK = "cloud.ibm.com/pause-check"
var PAUSE_BUILD = "cloud.ibm.com/pause-build"

type BuildRunEvent struct {
	Object struct {
		kube.KubeObject
		Spec   interface{}
		Status struct {
			CompletionTime string
			Conditions     []struct {
				Type   string
				Status string
			}
		}

		Code    int
		Message string
		Reason  string
	}

	Type string
}

// Process an event - which means a buildrun finished so we now need to
// find all knServices that are watching it and remove their PAUSE label
// so it can continue it's reconciliation processing (unless there are other
// PAUSE labels for other reasons)
func ProcessEvent(event *BuildRunEvent) {
	ns := event.Object.Metadata.Namespace
	name := event.Object.Metadata.Name
	uid := event.Object.Metadata.UID

	log.Printf("Processing %s: %s/%s uid:%s", event.Type, ns, name, uid)

	status := false
	for _, cond := range event.Object.Status.Conditions {
		if cond.Type == "Succeeded" && cond.Status == "True" {
			status = true
			break
		}
	}

	// Find all Revisions in this namespace that point to the buildrun
	labelSelector := "?labelSelector=" + PAUSE_BUILD + "=" + uid
	url := "/apis/serving.knative.dev/v1/namespaces/" + ns + "/revisions" +
		labelSelector

	rc, body, err := kube.KubeCall("GET", url, "")
	if rc != 200 {
		log.Printf("Error searching for a ksvc(%d): %s\n%s", rc, err, body)
		return
	}

	list := kube.KubeList{}
	if err = json.Unmarshal([]byte(body), &list); err != nil {
		log.Printf("Error decoding json: %s\n%s", err, body)
		return
	}

	// Now, for each, remove the label to unpause it
	patch := `{"metadata":{"labels":{"` + PAUSE_BUILD + `":null}}}`

	for _, item := range list.Items {
		// If the build didn't pass (or the buildrun was deleted) then
		// mark the revision appropriately before we unpause it
		if !status {
			// Mark the revision as failed due to bad build
			log.Printf("Set ksvc error build: %s/%s", ns, item.Metadata.Name)
			// TODO add the code!
		}

		// Now remove the label to unpause it
		log.Printf("Unpausing %s/%s", ns, item.Metadata.Name)
		url := "/apis/serving.knative.dev/v1/namespaces/" + ns +
			"/revisions/" + item.Metadata.Name
		rc, body, err = kube.KubeCall("PATCH", url, patch)
		if rc != 200 {
			log.Printf("Error unpausing: %s/%s: %d %s\n%s", ns,
				item.Metadata.Name, rc, err, body)
		}
	}

	// Add label to Buildrun so we don't process it again.
	// This needs to come last so that if we crash we'll process it again
	// to make sure we didn't miss any KnService updates.
	log.Printf("Patching buildrun: %s/%s", ns, name)
	patch = `{"metadata":{"labels":{"` + PAUSE_CHECK + `":"true"}}}`
	url = "/apis/shipwright.io/v1alpha1/namespaces/" + ns + "/buildruns/" + name
	rc, body, err = kube.KubeCall("PATCH", url, patch)
	if rc != 200 {
		log.Printf("Error patching buildrun %s/%s: %d %s\n%s", ns, name,
			rc, err, body)
	}
}

func main() {
	log.Printf("Starting buildrun pause watcher")

	ns := kube.Namespace
	if ns == "" {
		ns = "default"
	}

	// Skip resources that have already been processed by us.
	// We'll add a label to each buildrun as we process it to avoid
	// the overhead of processing them each time the watch stream is restarted.
	// Using a label allows us to also avoid the network overhead of etcd
	// sending us pointless events. I'd prefer to avoid updating the buildrun
	// but I couldn't find a reliable way to avoid the processing involved
	// with checking for knServices that might point to already processed
	// buildruns. It would be nice if the buildrun had a Status.annotations
	// that we could set and then avoid the risk of the user touching it.
	labelSelector := "&labelSelector=!" + PAUSE_CHECK

	for {
		log.Printf("Establishing watcher connection")
		url := "/apis/shipwright.io/v1alpha1/" +
			// "/buildruns?watch=true")  // Across all namespaces
			"namespaces/" + ns + "/buildruns?watch=true" + labelSelector

		rc, reader, err := kube.KubeStream("GET", url, "")
		if rc != 200 {
			log.Printf("Error from watcher: %d - %s", rc, err)
			os.Exit(1)
		}

		decoder := json.NewDecoder(reader)
		for decoder.More() {
			event := &BuildRunEvent{}

			if err := decoder.Decode(event); err != nil {
				log.Printf("Error decoding event: %s", err)
				os.Exit(1)
			}

			// str, _ := json.MarshalIndent(event, "", "  ")
			// log.Printf("Obj:\n%s", str)

			if event.Type == "ERROR" {
				log.Printf("Event Error:\n  %s\n  %s\n  %s",
					event.Object.Message, event.Object.Reason)
				os.Exit(1)
			}

			// Some DELETED events are sent due to the object being removed
			// from the event stream and NOT because the object was deleted.
			// Skip those cases.
			if event.Type == "DELETED" && event.Object.Metadata.DeletionTimestamp == "" {
				continue
			}

			// If the buildrun isn't being deleted and the build isn't done
			// yet, then skip it. If it's being deleted before the build is
			// done then we need to do our processing
			if event.Type != "DELETED" && event.Object.Status.CompletionTime == "" {
				continue
			}

			// Already processed this Build, skip it - should never happen
			if _, ok := event.Object.Metadata.Labels[PAUSE_CHECK]; ok {
				log.Printf("Already done: %s\n", event.Object.Metadata.Name)
				continue
			}

			// Look for paused knServices
			go ProcessEvent(event)
		}
	}
}
