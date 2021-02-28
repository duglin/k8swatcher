package main

import (
	"context"
	"log"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/kubernetes"
	_ "k8s.io/client-go/plugin/pkg/client/auth/oidc"
	"k8s.io/client-go/tools/clientcmd"
)

var client *kubernetes.Clientset
var ctx = context.Background()
var delOpts = metav1.DeleteOptions{}

var cmTimeMap = map[string]*Entry{} // ns/name -> *Entry
var cmList = []*Entry{}             // sorted by delTime
var queueMux = sync.Mutex{}

type Entry struct {
	nsName  string
	delTime time.Time
}

func PrintQueue() {
	log.Printf("  list: %d  map:%d", len(cmList), len(cmTimeMap))
	for i, entry := range cmList {
		log.Printf("  %-2d - %s %s", i, entry.nsName,
			entry.delTime.Format(time.RFC3339))
	}
}

func AddQueue(nsName string, delTime time.Time) {
	queueMux.Lock()
	defer queueMux.Unlock()

	entry := &Entry{
		nsName:  nsName,
		delTime: delTime,
	}

	e := cmTimeMap[nsName]
	if e != nil {
		// If already there, find it's delTime
		foundPos := sort.Search(len(cmList), func(i int) bool {
			listTime := cmList[i].delTime
			return listTime.Equal(e.delTime) || listTime.After(e.delTime)
		})
		if foundPos == -1 {
			panic("Add - queues are out of sync")
		}

		if cmList[foundPos].delTime == delTime {
			// already in right spot - just exit
			log.Printf("  No update to %q needed", nsName)
			return
		}

		// Remove it so we can add it in the right spot
		log.Printf("  Update to %q needed - removing", nsName)
		cmList = append(cmList[:foundPos], cmList[foundPos+1:]...)
		delete(cmTimeMap, nsName)
	}

	insert := sort.Search(len(cmList), func(i int) bool {
		listTime := cmList[i].delTime
		return listTime.Equal(entry.delTime) || listTime.After(entry.delTime)
	})

	if insert == -1 { // no existing entry is later, so append
		cmList = append(cmList, entry)
	} else {
		// insert into list
		cmList = append(cmList[:insert], append([]*Entry{entry}, cmList[insert:]...)...)
	}

	cmTimeMap[nsName] = entry
	log.Printf("  AddQueue: %s %s", nsName, delTime.Format(time.RFC3339))
	PrintQueue()
}

func DelQueue(nsName string) {
	entry := cmTimeMap[nsName]
	if entry != nil {
		delete(cmTimeMap, nsName)
		foundPos := sort.Search(len(cmList), func(i int) bool {
			listTime := cmList[i].delTime
			return listTime.Equal(entry.delTime) || listTime.After(entry.delTime)
		})
		if foundPos == -1 {
			PrintQueue()
			panic("DelQueues: Queues are out of sync")
		}
		cmList = append(cmList[:foundPos], cmList[foundPos+1:]...)
		log.Printf("  DelQueue: %s", nsName)
		PrintQueue()
	}
}

func deleteCM(ns string, name string) {
	log.Printf("  K8s deleting: %s/%s", ns, name)
	err := client.CoreV1().ConfigMaps(ns).Delete(ctx, name, delOpts)
	if err != nil {
		log.Printf("  Error deleting %s/%s: %s", ns, name, err)
	}
}

func processQueue() {
	for {
		now := time.Now()
		queueMux.Lock()
		for len(cmList) > 0 && now.After(cmList[0].delTime) {
			nsName := cmList[0].nsName
			DelQueue(nsName)                        // del no matter what
			parts := strings.SplitN(nsName, "/", 2) // ns, name
			if len(parts) == 2 {
				go deleteCM(parts[0], parts[1])
			}
		}
		queueMux.Unlock()
		time.Sleep(1 * time.Second)
	}
}

func main() {
	// pkg.go.dev/k8s.io/client-go/kubernetes
	// https://pkg.go.dev/k8s.io/client-go/rest
	// https://pkg.go.dev/k8s.io/client-go/tools/clientcmd
	log.Printf("Starting")

	// config, err := rest.InClusterConfig()
	config, err := clientcmd.BuildConfigFromFlags("", "/root/.kube/config")
	if err != nil {
		log.Fatalf("Error getting the cluster config: %s", err)
	}

	client, err = kubernetes.NewForConfig(config)
	if err != nil {
		log.Fatalf("Error setting up client: %s", err)
	}

	listOpts := metav1.ListOptions{
		LabelSelector: "autodelete", // true or delay in minutes
		Watch:         true,
	}

	go processQueue()

	// cms, err := client.CoreV1().ConfigMaps("").List(ctx, listOpts)
	// log.Printf("cms: %#q", cms)
	// listOpts.Watch = true

	for {
		cmWatcher, err := client.CoreV1().ConfigMaps("").Watch(ctx, listOpts)
		if err != nil {
			panic(err)
		}

		for {
			event := <-cmWatcher.ResultChan()
			if event.Type == "" {
				break
			}

			cm := event.Object.(*v1.ConfigMap)
			ns := cm.ObjectMeta.Namespace
			name := cm.ObjectMeta.Name
			nsName := ns + "/" + name
			log.Printf("Event: %v - %s", event.Type, nsName)

			if event.Type != watch.Added && event.Type != watch.Modified {
				continue // skip all others
			}

			createDate := cm.ObjectMeta.CreationTimestamp
			value := cm.ObjectMeta.Labels["autodelete"]

			// if 'true' or no createDate then blindly delete,else check it
			delTime := time.Now()
			if value != "true" && !createDate.IsZero() {
				val, err := strconv.Atoi(value)
				if err != nil {
					// Can't parse, play it safe and skip it
					log.Printf("  Error parsing delay %q: %s", value, err)
					continue
				}

				delTime = createDate.Add(time.Duration(val) * time.Minute)
			}
			AddQueue(nsName, delTime)
		}
	}
}
