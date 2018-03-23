// Copyright 2016 Gravitational Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//    http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package rigging

import (
	"context"

	"github.com/gravitational/trace"

	log "github.com/Sirupsen/logrus"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/pkg/api/v1"
	batchv1 "k8s.io/client-go/pkg/apis/batch/v1"
)

func NewJobControl(config JobConfig) (*JobControl, error) {
	err := config.checkAndSetDefaults()
	if err != nil {
		return nil, trace.Wrap(err)
	}

	return &JobControl{
		JobConfig: config,
		Entry: log.WithFields(log.Fields{
			"job": formatMeta(config.Job.ObjectMeta),
		}),
	}, nil
}

func (c *JobControl) Delete(ctx context.Context, cascade bool) error {
	c.Infof("delete %v", formatMeta(c.Job.ObjectMeta))

	jobs := c.Batch().Jobs(c.Job.Namespace)
	currentJob, err := jobs.Get(c.Job.Name, metav1.GetOptions{})
	if err != nil {
		return ConvertError(err)
	}

	pods := c.Core().Pods(c.Job.Namespace)
	currentPods, err := c.collectPods(currentJob)
	if err != nil {
		return trace.Wrap(err)
	}

	c.Info("deleting current job")
	deletePolicy := metav1.DeletePropagationForeground
	err = jobs.Delete(c.Job.Name, &metav1.DeleteOptions{
		PropagationPolicy: &deletePolicy,
	})
	if err != nil {
		return ConvertError(err)
	}

	err = waitForObjectDeletion(func() error {
		_, err := jobs.Get(c.Job.Name, metav1.GetOptions{})
		return ConvertError(err)
	})
	if err != nil {
		return trace.Wrap(err)
	}

	if !cascade {
		c.Info("cascade not set, returning")
	}
	err = deletePods(pods, currentPods, *c.Entry)
	return trace.Wrap(err)
}

func (c *JobControl) Upsert(ctx context.Context) error {
	c.Infof("upsert %v", formatMeta(c.Job.ObjectMeta))

	jobs := c.Batch().Jobs(c.Job.Namespace)
	currentJob, err := jobs.Get(c.Job.Name, metav1.GetOptions{})
	err = ConvertError(err)
	if err != nil {
		if !trace.IsNotFound(err) {
			return trace.Wrap(err)
		}
		// Get always returns an object
		currentJob = nil
	}

	if currentJob != nil {
		control, err := NewJobControl(JobConfig{Job: currentJob, Clientset: c.Clientset})
		if err != nil {
			return ConvertError(err)
		}
		err = control.Delete(ctx, true)
		if err != nil {
			return ConvertError(err)
		}
	}

	c.Info("creating new job")
	c.Job.UID = ""
	c.Job.SelfLink = ""
	c.Job.ResourceVersion = ""
	if c.Job.Spec.Selector != nil {
		// Remove auto-generated labels
		delete(c.Job.Spec.Selector.MatchLabels, ControllerUIDLabel)
		delete(c.Job.Spec.Template.Labels, ControllerUIDLabel)
	}

	err = withExponentialBackoff(func() error {
		_, err := jobs.Create(c.Job)
		return ConvertError(err)
	})
	return trace.Wrap(err)
}

func (c *JobControl) Status() error {
	jobs := c.Batch().Jobs(c.Job.Namespace)
	job, err := jobs.Get(c.Job.Name, metav1.GetOptions{})
	if err != nil {
		return ConvertError(err)
	}

	succeeded := job.Status.Succeeded
	active := job.Status.Active
	var complete bool
	if job.Spec.Completions == nil {
		// This type of job is complete when any pod exits with success
		if succeeded > 0 && active == 0 {
			complete = true
		}
	} else {
		// Job specifies a number of completions
		completions := *job.Spec.Completions
		if succeeded >= completions {
			complete = true
		}
	}

	if !complete {
		return trace.CompareFailed("job %v not yet complete (succeeded: %v, active: %v)",
			formatMeta(job.ObjectMeta), succeeded, active)
	}
	return nil
}

func (c *JobControl) collectPods(job *batchv1.Job) (map[string]v1.Pod, error) {
	var labels map[string]string
	if job.Spec.Selector != nil {
		labels = job.Spec.Selector.MatchLabels
	}
	pods, err := CollectPods(job.Namespace, labels, c.Entry, c.Clientset, func(ref metav1.OwnerReference) bool {
		return ref.Kind == KindJob && ref.UID == job.UID
	})
	return pods, ConvertError(err)
}

type JobControl struct {
	JobConfig
	*log.Entry
}

type JobConfig struct {
	Job *batchv1.Job
	*kubernetes.Clientset
}

func (c *JobConfig) checkAndSetDefaults() error {
	if c.Clientset == nil {
		return trace.BadParameter("missing parameter Clientset")
	}
	c.Job.Kind = KindJob
	if c.Job.APIVersion == "" {
		c.Job.APIVersion = BatchAPIVersion
	}
	return nil
}
