/*
Copyright 2016 The Kubernetes Authors.

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

package apimachinery

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/onsi/ginkgo"
	"github.com/prometheus/common/expfmt"
	"github.com/prometheus/common/model"

	flowcontrol "k8s.io/api/flowcontrol/v1beta1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apiserver/pkg/authentication/user"
	"k8s.io/client-go/rest"
	"k8s.io/kubernetes/test/e2e/framework"
)

const (
	requestConcurrencyLimitMetricName      = "apiserver_flowcontrol_request_concurrency_limit"
	requestConcurrencyLimitMetricLabelName = "priority_level"
)

var _ = SIGDescribe("API priority and fairness", func() {
	f := framework.NewDefaultFramework("flowschemas")

	ginkgo.It("should ensure that requests can be classified by testing flow-schemas/priority-levels", func() {
		testingFlowSchemaName := "e2e-testing-flowschema"
		testingPriorityLevelName := "e2e-testing-prioritylevel"
		matchingUsername := "noxu"
		nonMatchingUsername := "foo"

		ginkgo.By("creating a testing prioritylevel")
		createdPriorityLevel, cleanup := createPriorityLevel(f, testingPriorityLevelName, 1)
		defer cleanup()

		ginkgo.By("creating a testing flowschema")
		createdFlowSchema, cleanup := createFlowSchema(f, testingFlowSchemaName, 1000, testingPriorityLevelName, matchingUsername)
		defer cleanup()

		ginkgo.By("checking response headers contain flow-schema/priority-level uid")
		if !testResponseHeaderMatches(f, matchingUsername, string(createdPriorityLevel.UID), string(createdFlowSchema.UID)) {
			framework.Failf("matching user doesnt received UID for the testing priority-level and flow-schema")
		}
		if testResponseHeaderMatches(f, nonMatchingUsername, string(createdPriorityLevel.UID), string(createdPriorityLevel.UID)) {
			framework.Failf("non-matching user unexpectedly received UID for the testing priority-level and flow-schema")
		}
	})

	// This test creates two flow schemas and a corresponding priority level for
	// each flow schema. One flow schema has a higher match precedence. With two
	// clients making requests at different rates, we test to make sure that the
	// higher QPS client cannot drown out the other one despite having higher
	// priority.
	ginkgo.It("should ensure that requests can't be drowned out (priority)", func() {
		flowSchemaNamePrefix := "e2e-testing-flowschema"
		priorityLevelNamePrefix := "e2e-testing-prioritylevel"
		loadDuration := 10 * time.Second
		type client struct {
			username              string
			qps                   float64
			priorityLevelName     string
			concurrencyMultiplier float64
			concurrency           int32
			flowSchemaName        string
			matchingPrecedence    int32
			completedRequests     int32
		}
		clients := []client{
			// "highqps" refers to a client that creates requests at a much higher
			// QPS than its counter-part and well above its concurrency share limit.
			// In contrast, "lowqps" stays under its concurrency shares.
			// Additionally, the "highqps" client also has a higher matching
			// precedence for its flow schema.
			{username: "highqps", qps: 100.0, concurrencyMultiplier: 2.0, matchingPrecedence: 999},
			{username: "lowqps", qps: 5.0, concurrencyMultiplier: 0.5, matchingPrecedence: 1000},
		}

		ginkgo.By("creating test priority levels and flow schemas")
		for i := range clients {
			clients[i].priorityLevelName = fmt.Sprintf("%s-%s", priorityLevelNamePrefix, clients[i].username)
			framework.Logf("creating PriorityLevel %q", clients[i].priorityLevelName)
			_, cleanup := createPriorityLevel(f, clients[i].priorityLevelName, 1)
			defer cleanup()

			clients[i].flowSchemaName = fmt.Sprintf("%s-%s", flowSchemaNamePrefix, clients[i].username)
			framework.Logf("creating FlowSchema %q", clients[i].flowSchemaName)
			_, cleanup = createFlowSchema(f, clients[i].flowSchemaName, clients[i].matchingPrecedence, clients[i].priorityLevelName, clients[i].username)
			defer cleanup()
		}

		ginkgo.By("getting request concurrency from metrics")
		for i := range clients {
			realConcurrency := getPriorityLevelConcurrency(f, clients[i].priorityLevelName)
			clients[i].concurrency = int32(float64(realConcurrency) * clients[i].concurrencyMultiplier)
			if clients[i].concurrency < 1 {
				clients[i].concurrency = 1
			}
			framework.Logf("request concurrency for %q will be %d (concurrency share = %d)", clients[i].username, clients[i].concurrency, realConcurrency)
		}

		ginkgo.By(fmt.Sprintf("starting uniform QPS load for %s", loadDuration.String()))
		var wg sync.WaitGroup
		for i := range clients {
			wg.Add(1)
			go func(c *client) {
				defer wg.Done()
				framework.Logf("starting uniform QPS load for %q: concurrency=%d, qps=%.1f", c.username, c.concurrency, c.qps)
				c.completedRequests = uniformQPSLoadConcurrent(f, c.username, c.concurrency, c.qps, loadDuration)
			}(&clients[i])
		}
		wg.Wait()

		ginkgo.By("checking completed requests with expected values")
		for _, client := range clients {
			// Each client should have 95% of its ideal number of completed requests.
			maxCompletedRequests := float64(client.concurrency) * client.qps * float64(loadDuration/time.Second)
			fractionCompleted := float64(client.completedRequests) / maxCompletedRequests
			framework.Logf("client %q completed %d/%d requests (%.1f%%)", client.username, client.completedRequests, int32(maxCompletedRequests), 100*fractionCompleted)
			if fractionCompleted < 0.95 {
				framework.Failf("client %q: got %.1f%% completed requests, want at least 95%%", client.username, 100*fractionCompleted)
			}
		}
	})

	// This test has two clients (different usernames) making requests at
	// different rates. Both clients' requests get mapped to the same flow schema
	// and priority level. We expect APF's "ByUser" flow distinguisher to isolate
	// the two clients and not allow one client to drown out the other despite
	// having a higher QPS.
	ginkgo.It("should ensure that requests can't be drowned out (fairness)", func() {
		priorityLevelName := "e2e-testing-prioritylevel"
		flowSchemaName := "e2e-testing-flowschema"
		loadDuration := 10 * time.Second

		framework.Logf("creating PriorityLevel %q", priorityLevelName)
		_, cleanup := createPriorityLevel(f, priorityLevelName, 1)
		defer cleanup()

		framework.Logf("creating FlowSchema %q", flowSchemaName)
		_, cleanup = createFlowSchema(f, flowSchemaName, 1000, priorityLevelName, "*")
		defer cleanup()

		type client struct {
			username              string
			qps                   float64
			concurrencyMultiplier float64
			concurrency           int32
			completedRequests     int32
		}
		clients := []client{
			{username: "highqps", qps: 100.0, concurrencyMultiplier: 2.0},
			{username: "lowqps", qps: 5.0, concurrencyMultiplier: 0.5},
		}

		framework.Logf("getting real concurrency")
		realConcurrency := getPriorityLevelConcurrency(f, priorityLevelName)
		for i := range clients {
			clients[i].concurrency = int32(float64(realConcurrency) * clients[i].concurrencyMultiplier)
			if clients[i].concurrency < 1 {
				clients[i].concurrency = 1
			}
			framework.Logf("request concurrency for %q will be %d", clients[i].username, clients[i].concurrency)
		}

		ginkgo.By(fmt.Sprintf("starting uniform QPS load for %s", loadDuration.String()))
		var wg sync.WaitGroup
		for i := range clients {
			wg.Add(1)
			go func(c *client) {
				defer wg.Done()
				framework.Logf("starting uniform QPS load for %q: concurrency=%d, qps=%.1f", c.username, c.concurrency, c.qps)
				c.completedRequests = uniformQPSLoadConcurrent(f, c.username, c.concurrency, c.qps, loadDuration)
			}(&clients[i])
		}
		wg.Wait()

		ginkgo.By("checking completed requests with expected values")
		for _, client := range clients {
			// Each client should have 95% of its ideal number of completed requests.
			maxCompletedRequests := float64(client.concurrency) * client.qps * float64(loadDuration/time.Second)
			fractionCompleted := float64(client.completedRequests) / maxCompletedRequests
			framework.Logf("client %q completed %d/%d requests (%.1f%%)", client.username, client.completedRequests, int32(maxCompletedRequests), 100*fractionCompleted)
			if fractionCompleted < 0.95 {
				framework.Failf("client %q: got %.1f%% completed requests, want at least 95%%", client.username, 100*fractionCompleted)
			}
		}
	})
})

// createPriorityLevel creates a priority level with the provided assured
// concurrency share.
func createPriorityLevel(f *framework.Framework, priorityLevelName string, assuredConcurrencyShares int32) (*flowcontrol.PriorityLevelConfiguration, func()) {
	createdPriorityLevel, err := f.ClientSet.FlowcontrolV1beta1().PriorityLevelConfigurations().Create(
		context.TODO(),
		&flowcontrol.PriorityLevelConfiguration{
			ObjectMeta: metav1.ObjectMeta{
				Name: priorityLevelName,
			},
			Spec: flowcontrol.PriorityLevelConfigurationSpec{
				Type: flowcontrol.PriorityLevelEnablementLimited,
				Limited: &flowcontrol.LimitedPriorityLevelConfiguration{
					AssuredConcurrencyShares: assuredConcurrencyShares,
					LimitResponse: flowcontrol.LimitResponse{
						Type: flowcontrol.LimitResponseTypeReject,
					},
				},
			},
		},
		metav1.CreateOptions{})
	framework.ExpectNoError(err)
	return createdPriorityLevel, func() {
		framework.ExpectNoError(f.ClientSet.FlowcontrolV1beta1().PriorityLevelConfigurations().Delete(context.TODO(), priorityLevelName, metav1.DeleteOptions{}))
	}
}

func getPriorityLevelConcurrency(f *framework.Framework, priorityLevelName string) int32 {
	resp, err := f.ClientSet.CoreV1().RESTClient().Get().RequestURI("/metrics").DoRaw(context.TODO())
	framework.ExpectNoError(err)
	sampleDecoder := expfmt.SampleDecoder{
		Dec:  expfmt.NewDecoder(bytes.NewBuffer(resp), expfmt.FmtText),
		Opts: &expfmt.DecodeOptions{},
	}
	for {
		var v model.Vector
		err := sampleDecoder.Decode(&v)
		if err == io.EOF {
			break
		}
		framework.ExpectNoError(err)
		for _, metric := range v {
			if string(metric.Metric[model.MetricNameLabel]) != requestConcurrencyLimitMetricName {
				continue
			}
			if string(metric.Metric[requestConcurrencyLimitMetricLabelName]) != priorityLevelName {
				continue
			}
			return int32(metric.Value)
		}
	}
	framework.ExpectNoError(fmt.Errorf("cannot find metric %q with matching priority level name label %q", requestConcurrencyLimitMetricName, priorityLevelName))
	return 0
}

// createFlowSchema creates a flow schema referring to a particular priority
// level and matching the username provided.
func createFlowSchema(f *framework.Framework, flowSchemaName string, matchingPrecedence int32, priorityLevelName string, matchingUsername string) (*flowcontrol.FlowSchema, func()) {
	var subjects []flowcontrol.Subject
	if matchingUsername == "*" {
		subjects = append(subjects, flowcontrol.Subject{
			Kind: flowcontrol.SubjectKindGroup,
			Group: &flowcontrol.GroupSubject{
				Name: user.AllAuthenticated,
			},
		})
	} else {
		subjects = append(subjects, flowcontrol.Subject{
			Kind: flowcontrol.SubjectKindUser,
			User: &flowcontrol.UserSubject{
				Name: matchingUsername,
			},
		})
	}

	createdFlowSchema, err := f.ClientSet.FlowcontrolV1beta1().FlowSchemas().Create(
		context.TODO(),
		&flowcontrol.FlowSchema{
			ObjectMeta: metav1.ObjectMeta{
				Name: flowSchemaName,
			},
			Spec: flowcontrol.FlowSchemaSpec{
				MatchingPrecedence: matchingPrecedence,
				PriorityLevelConfiguration: flowcontrol.PriorityLevelConfigurationReference{
					Name: priorityLevelName,
				},
				DistinguisherMethod: &flowcontrol.FlowDistinguisherMethod{
					Type: flowcontrol.FlowDistinguisherMethodByUserType,
				},
				Rules: []flowcontrol.PolicyRulesWithSubjects{
					{
						Subjects: subjects,
						NonResourceRules: []flowcontrol.NonResourcePolicyRule{
							{
								Verbs:           []string{flowcontrol.VerbAll},
								NonResourceURLs: []string{flowcontrol.NonResourceAll},
							},
						},
					},
				},
			},
		},
		metav1.CreateOptions{})
	framework.ExpectNoError(err)
	return createdFlowSchema, func() {
		framework.ExpectNoError(f.ClientSet.FlowcontrolV1beta1().FlowSchemas().Delete(context.TODO(), flowSchemaName, metav1.DeleteOptions{}))
	}
}

// makeRequests creates a request to the API server and returns the response.
func makeRequest(f *framework.Framework, username string) *http.Response {
	config := f.ClientConfig()
	config.Impersonate.UserName = username
	config.Impersonate.Groups = []string{"system:authenticated"}
	roundTripper, err := rest.TransportFor(config)
	framework.ExpectNoError(err)

	req, err := http.NewRequest(http.MethodGet, f.ClientSet.CoreV1().RESTClient().Get().AbsPath("version").URL().String(), nil)
	framework.ExpectNoError(err)

	response, err := roundTripper.RoundTrip(req)
	framework.ExpectNoError(err)
	return response
}

func testResponseHeaderMatches(f *framework.Framework, impersonatingUser, plUID, fsUID string) bool {
	response := makeRequest(f, impersonatingUser)
	if response.Header.Get(flowcontrol.ResponseHeaderMatchedFlowSchemaUID) != fsUID {
		return false
	}
	if response.Header.Get(flowcontrol.ResponseHeaderMatchedPriorityLevelConfigurationUID) != plUID {
		return false
	}
	return true
}

// uniformQPSLoadSingle loads the API server with requests at a uniform <qps>
// for <loadDuration> time. The number of successfully completed requests is
// returned.
func uniformQPSLoadSingle(f *framework.Framework, username string, qps float64, loadDuration time.Duration) int32 {
	var completed int32
	var wg sync.WaitGroup
	ticker := time.NewTicker(time.Duration(1e9/qps) * time.Nanosecond)
	defer ticker.Stop()
	timer := time.NewTimer(loadDuration)
	for {
		select {
		case <-ticker.C:
			wg.Add(1)
			// Each request will have a non-zero latency. In addition, there may be
			// multiple concurrent requests in-flight. As a result, a request may
			// take longer than the time between two different consecutive ticks
			// regardless of whether a requests is accepted or rejected. For example,
			// in cases with clients making requests far above their concurrency
			// share, with little time between consecutive requests, due to limited
			// concurrency, newer requests will be enqueued until older ones
			// complete. Hence the synchronisation with sync.WaitGroup.
			go func() {
				defer wg.Done()
				makeRequest(f, username)
				atomic.AddInt32(&completed, 1)
			}()
		case <-timer.C:
			// Still in-flight requests should not contribute to the completed count.
			totalCompleted := atomic.LoadInt32(&completed)
			wg.Wait() // do not leak goroutines
			return totalCompleted
		}
	}
}

// uniformQPSLoadConcurrent loads the API server with a <concurrency> number of
// clients impersonating to be <username>, each creating requests at a uniform
// rate defined by <qps>. The sum of number of successfully completed requests
// across all concurrent clients is returned.
func uniformQPSLoadConcurrent(f *framework.Framework, username string, concurrency int32, qps float64, loadDuration time.Duration) int32 {
	var completed int32
	var wg sync.WaitGroup
	wg.Add(int(concurrency))
	for i := int32(0); i < concurrency; i++ {
		go func() {
			defer wg.Done()
			atomic.AddInt32(&completed, uniformQPSLoadSingle(f, username, qps, loadDuration))
		}()
	}
	wg.Wait()
	return completed
}
