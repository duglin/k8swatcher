# K8sWatcher

Just watches Kube configMaps for a "autodelete" label and then will delete
the configMap if present (after the delay, in minutes, as specified on 
the label).

See `testit`.
