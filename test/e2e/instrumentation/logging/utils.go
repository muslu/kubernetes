/*
Copyright 2017 The Kubernetes Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package logging

import (
	"fmt"
	"regexp"
	"strconv"
	"time"

	api_v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	meta_v1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/client-go/util/integer"
	"k8s.io/kubernetes/pkg/api"
	"k8s.io/kubernetes/test/e2e/framework"
)

const (
	// Duration of delay between any two attempts to check if all logs are ingested
	ingestionRetryDelay = 30 * time.Second

	// Amount of requested cores for logging container in millicores
	loggingContainerCpuRequest = 10

	// Amount of requested memory for logging container in bytes
	loggingContainerMemoryRequest = 10 * 1024 * 1024

	// Name of the container used for logging tests
	loggingContainerName = "logging-container"
)

var (
	// Regexp, matching the contents of log entries, parsed or not
	logEntryMessageRegex = regexp.MustCompile("(?:I\\d+ \\d+:\\d+:\\d+.\\d+       \\d+ logs_generator.go:67] )?(\\d+) .*")
)

type logEntry struct {
	Payload string
}

func (entry logEntry) getLogEntryNumber() (int, bool) {
	submatch := logEntryMessageRegex.FindStringSubmatch(entry.Payload)
	if submatch == nil || len(submatch) < 2 {
		return 0, false
	}

	lineNumber, err := strconv.Atoi(submatch[1])
	return lineNumber, err == nil
}

type logsProvider interface {
	Init() error
	Cleanup()
	ReadEntries(*loggingPod) []logEntry
	FluentdApplicationName() string
}

type loggingTestConfig struct {
	LogsProvider              logsProvider
	Pods                      []*loggingPod
	IngestionTimeout          time.Duration
	MaxAllowedLostFraction    float64
	MaxAllowedFluentdRestarts int
}

// Type to track the progress of logs generating pod
type loggingPod struct {
	// Name equals to the pod name and the container name.
	Name string
	// NodeName is the name of the node this pod will be
	// assigned to. Can be empty.
	NodeName string
	// Occurrences is a cache of ingested and read entries.
	Occurrences map[int]logEntry
	// ExpectedLinesNumber is the number of lines that are
	// expected to be ingested from this pod.
	ExpectedLinesNumber int
	// RunDuration is how long the pod will live.
	RunDuration time.Duration
}

func newLoggingPod(podName string, nodeName string, totalLines int, loggingDuration time.Duration) *loggingPod {
	return &loggingPod{
		Name:                podName,
		NodeName:            nodeName,
		Occurrences:         make(map[int]logEntry),
		ExpectedLinesNumber: totalLines,
		RunDuration:         loggingDuration,
	}
}

func (p *loggingPod) Start(f *framework.Framework) {
	framework.Logf("Starting pod %s", p.Name)
	f.PodClient().Create(&api_v1.Pod{
		ObjectMeta: meta_v1.ObjectMeta{
			Name: p.Name,
		},
		Spec: api_v1.PodSpec{
			RestartPolicy: api_v1.RestartPolicyNever,
			Containers: []api_v1.Container{
				{
					Name:  loggingContainerName,
					Image: "gcr.io/google_containers/logs-generator:v0.1.0",
					Env: []api_v1.EnvVar{
						{
							Name:  "LOGS_GENERATOR_LINES_TOTAL",
							Value: strconv.Itoa(p.ExpectedLinesNumber),
						},
						{
							Name:  "LOGS_GENERATOR_DURATION",
							Value: p.RunDuration.String(),
						},
					},
					Resources: api_v1.ResourceRequirements{
						Requests: api_v1.ResourceList{
							api_v1.ResourceCPU: *resource.NewMilliQuantity(
								loggingContainerCpuRequest,
								resource.DecimalSI),
							api_v1.ResourceMemory: *resource.NewQuantity(
								loggingContainerMemoryRequest,
								resource.BinarySI),
						},
					},
				},
			},
			NodeName: p.NodeName,
		},
	})
}

func startNewLoggingPod(f *framework.Framework, podName string, nodeName string, totalLines int, loggingDuration time.Duration) *loggingPod {
	pod := newLoggingPod(podName, nodeName, totalLines, loggingDuration)
	pod.Start(f)
	return pod
}

func waitForSomeLogs(f *framework.Framework, config *loggingTestConfig) error {
	podHasIngestedLogs := make([]bool, len(config.Pods))
	podWithIngestedLogsCount := 0

	for start := time.Now(); time.Since(start) < config.IngestionTimeout; time.Sleep(ingestionRetryDelay) {
		for podIdx, pod := range config.Pods {
			if podHasIngestedLogs[podIdx] {
				continue
			}

			entries := config.LogsProvider.ReadEntries(pod)
			if len(entries) == 0 {
				framework.Logf("No log entries from pod %s", pod.Name)
				continue
			}

			for _, entry := range entries {
				if _, ok := entry.getLogEntryNumber(); ok {
					framework.Logf("Found some log entries from pod %s", pod.Name)
					podHasIngestedLogs[podIdx] = true
					podWithIngestedLogsCount++
					break
				}
			}
		}

		if podWithIngestedLogsCount == len(config.Pods) {
			break
		}
	}

	if podWithIngestedLogsCount < len(config.Pods) {
		return fmt.Errorf("some logs were ingested for %d pods out of %d", podWithIngestedLogsCount, len(config.Pods))
	}

	return nil
}

func waitForFullLogsIngestion(f *framework.Framework, config *loggingTestConfig) error {
	expectedLinesNumber := 0
	for _, pod := range config.Pods {
		expectedLinesNumber += pod.ExpectedLinesNumber
	}

	totalMissing := expectedLinesNumber

	missingByPod := make([]int, len(config.Pods))
	for podIdx, pod := range config.Pods {
		missingByPod[podIdx] = pod.ExpectedLinesNumber
	}

	for start := time.Now(); time.Since(start) < config.IngestionTimeout; time.Sleep(ingestionRetryDelay) {
		missing := 0
		for podIdx, pod := range config.Pods {
			if missingByPod[podIdx] == 0 {
				continue
			}

			missingByPod[podIdx] = pullMissingLogsCount(config.LogsProvider, pod)
			missing += missingByPod[podIdx]
		}

		totalMissing = missing
		if totalMissing > 0 {
			framework.Logf("Still missing %d lines in total", totalMissing)
		} else {
			break
		}
	}

	lostFraction := float64(totalMissing) / float64(expectedLinesNumber)

	if totalMissing > 0 {
		framework.Logf("After %v still missing %d lines, %.2f%% of total number of lines",
			config.IngestionTimeout, totalMissing, lostFraction*100)
		for podIdx, missing := range missingByPod {
			if missing != 0 {
				framework.Logf("Still missing %d lines for pod %s", missing, config.Pods[podIdx].Name)
			}
		}
	}

	if lostFraction > config.MaxAllowedLostFraction {
		return fmt.Errorf("lost %.2f%% of lines, but only loss of %.2f%% can be tolerated",
			lostFraction*100, config.MaxAllowedLostFraction*100)
	}

	fluentdPods, err := getFluentdPods(f, config.LogsProvider.FluentdApplicationName())
	if err != nil {
		return fmt.Errorf("failed to get fluentd pods due to %v", err)
	}

	maxRestartCount := 0
	for _, fluentdPod := range fluentdPods.Items {
		restartCount := int(fluentdPod.Status.ContainerStatuses[0].RestartCount)
		maxRestartCount = integer.IntMax(maxRestartCount, restartCount)

		framework.Logf("Fluentd pod %s on node %s was restarted %d times",
			fluentdPod.Name, fluentdPod.Spec.NodeName, restartCount)
	}

	if maxRestartCount > config.MaxAllowedFluentdRestarts {
		return fmt.Errorf("max fluentd pod restarts was %d, which is more than allowed %d",
			maxRestartCount, config.MaxAllowedFluentdRestarts)
	}

	return nil
}

func pullMissingLogsCount(logsProvider logsProvider, pod *loggingPod) int {
	missingOnPod, err := getMissingLinesCount(logsProvider, pod)
	if err != nil {
		framework.Logf("Failed to get missing lines count from pod %s due to %v", pod.Name, err)
		return pod.ExpectedLinesNumber
	}

	return missingOnPod
}

func getMissingLinesCount(logsProvider logsProvider, pod *loggingPod) (int, error) {
	entries := logsProvider.ReadEntries(pod)

	for _, entry := range entries {
		lineNumber, ok := entry.getLogEntryNumber()
		if !ok {
			continue
		}

		if lineNumber < 0 || lineNumber >= pod.ExpectedLinesNumber {
			framework.Logf("Unexpected line number: %d", lineNumber)
		} else {
			pod.Occurrences[lineNumber] = entry
		}
	}

	return pod.ExpectedLinesNumber - len(pod.Occurrences), nil
}

func ensureSingleFluentdOnEachNode(f *framework.Framework, fluentdApplicationName string) error {
	fluentdPodList, err := getFluentdPods(f, fluentdApplicationName)
	if err != nil {
		return err
	}

	fluentdPodsPerNode := make(map[string]int)
	for _, fluentdPod := range fluentdPodList.Items {
		fluentdPodsPerNode[fluentdPod.Spec.NodeName]++
	}

	nodeList := framework.GetReadySchedulableNodesOrDie(f.ClientSet)
	for _, node := range nodeList.Items {
		fluentdPodCount, ok := fluentdPodsPerNode[node.Name]

		if !ok {
			return fmt.Errorf("node %s doesn't have fluentd instance", node.Name)
		} else if fluentdPodCount != 1 {
			return fmt.Errorf("node %s contains %d fluentd instaces, expected exactly one", node.Name, fluentdPodCount)
		}
	}

	return nil
}

func getFluentdPods(f *framework.Framework, fluentdApplicationName string) (*api_v1.PodList, error) {
	label := labels.SelectorFromSet(labels.Set(map[string]string{"k8s-app": fluentdApplicationName}))
	options := meta_v1.ListOptions{LabelSelector: label.String()}
	return f.ClientSet.Core().Pods(api.NamespaceSystem).List(options)
}
