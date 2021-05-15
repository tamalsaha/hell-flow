package kube

import (
	"io"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/pkg/errors"
	"helm.sh/helm/v3/pkg/kube"
	v1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/cli-runtime/pkg/genericclioptions"
	"k8s.io/cli-runtime/pkg/resource"
	cmdutil "k8s.io/kubectl/pkg/cmd/util"
	"kmodules.xyz/apply"
)

type Client struct {
	*kube.Client
	applier *apply.ApplyOptions
}

var _ kube.Interface = &Client{}

func New(getter genericclioptions.RESTClientGetter, log func(string, ...interface{})) (*Client, error) {
	kc := kube.New(getter)
	kc.Log = log

	applyOptions := apply.NewApplyOptions(genericclioptions.IOStreams{
		In:     os.Stdin,
		Out:    os.Stdout,
		ErrOut: os.Stderr,
	})
	err := applyOptions.Complete(kc.Factory.(cmdutil.Factory))
	if err != nil {
		return nil, err
	}

	return &Client{
		Client:  kc,
		applier: applyOptions,
	}, nil
}

func (c *Client) Create(resources kube.ResourceList) (*kube.Result, error) {
	c.Log("creating %d resource(s)", len(resources))
	if err := perform(resources, c.applyResource); err != nil {
		return nil, err
	}
	return &kube.Result{Created: resources}, nil
}

func (c *Client) Wait(resources kube.ResourceList, timeout time.Duration) error {
	return c.Client.Wait(resources, timeout)
}

func (c *Client) WaitWithJobs(resources kube.ResourceList, timeout time.Duration) error {
	return c.Client.WaitWithJobs(resources, timeout)
}

func (c *Client) Delete(resources kube.ResourceList) (*kube.Result, []error) {
	return c.Client.Delete(resources)
}

func (c *Client) WatchUntilReady(resources kube.ResourceList, timeout time.Duration) error {
	return c.Client.WatchUntilReady(resources, timeout)
}

var metadataAccessor = meta.NewAccessor()

// Update takes the current list of objects and target list of objects and
// creates resources that don't already exist, updates resources that have been
// modified in the target configuration, and deletes resources from the current
// configuration that are not present in the target configuration. If an error
// occurs, a Result will still be returned with the error, containing all
// resource updates, creations, and deletions that were attempted. These can be
// used for cleanup or other logging purposes.
func (c *Client) Update(original_nee_current, target kube.ResourceList, force bool) (*kube.Result, error) {
	updateErrors := []string{}
	res := &kube.Result{}

	c.Log("checking %d resources for changes", len(target))
	err := target.Visit(func(info *resource.Info, err error) error {
		if err != nil {
			return err
		}

		helper := resource.NewHelper(info.Client, info.Mapping)
		if _, err := helper.Get(info.Namespace, info.Name); err != nil {
			if !apierrors.IsNotFound(err) {
				return errors.Wrap(err, "could not get information about the resource")
			}

			// Append the created resource to the results, even if something fails
			res.Created = append(res.Created, info)

			// Since the resource does not exist, create it.
			if err := c.applyResource(info); err != nil {
				return errors.Wrap(err, "failed to create resource")
			}

			kind := info.Mapping.GroupVersionKind.Kind
			c.Log("Created a new %s called %q in %s\n", kind, info.Name, info.Namespace)
			return nil
		}

		originalInfo := original_nee_current.Get(info)
		if originalInfo == nil {
			kind := info.Mapping.GroupVersionKind.Kind
			return errors.Errorf("no %s with the name %q found", kind, info.Name)
		}

		// WARNING: Replaced by applier
		//if err := updateResource(c, info, originalInfo.Object, force); err != nil {
		//	c.Log("error updating the resource %q:\n\t %v", info.Name, err)
		//	updateErrors = append(updateErrors, err.Error())
		//}
		if err := c.applyResource(info); err != nil {
			c.Log("error updating the resource %q:\n\t %v", info.Name, err)
			updateErrors = append(updateErrors, err.Error())
		}
		// Because we check for errors later, append the info regardless
		res.Updated = append(res.Updated, info)

		return nil
	})

	switch {
	case err != nil:
		return res, err
	case len(updateErrors) != 0:
		return res, errors.Errorf(strings.Join(updateErrors, " && "))
	}

	for _, info := range original_nee_current.Difference(target) {
		c.Log("Deleting %q in %s...", info.Name, info.Namespace)

		if err := info.Get(); err != nil {
			c.Log("Unable to get obj %q, err: %s", info.Name, err)
			continue
		}
		annotations, err := metadataAccessor.Annotations(info.Object)
		if err != nil {
			c.Log("Unable to get annotations on %q, err: %s", info.Name, err)
		}
		if annotations != nil && annotations[kube.ResourcePolicyAnno] == kube.KeepPolicy {
			c.Log("Skipping delete of %q due to annotation [%s=%s]", info.Name, kube.ResourcePolicyAnno, kube.KeepPolicy)
			continue
		}
		if err := deleteResource(info); err != nil {
			c.Log("Failed to delete %q, err: %s", info.ObjectName(), err)
			continue
		}
		res.Deleted = append(res.Deleted, info)
	}
	return res, nil
}

func (c *Client) Build(reader io.Reader, validate bool) (kube.ResourceList, error) {
	return c.Client.Build(reader, validate)
}

func (c *Client) WaitAndGetCompletedPodPhase(name string, timeout time.Duration) (v1.PodPhase, error) {
	return c.Client.WaitAndGetCompletedPodPhase(name, timeout)
}

func (c *Client) IsReachable() error {
	return c.Client.IsReachable()
}

// -- COPY from HElM

func perform(infos kube.ResourceList, fn func(*resource.Info) error) error {
	if len(infos) == 0 {
		return kube.ErrNoObjectsVisited
	}

	errs := make(chan error)
	go batchPerform(infos, fn, errs)

	for range infos {
		err := <-errs
		if err != nil {
			return err
		}
	}
	return nil
}

func batchPerform(infos kube.ResourceList, fn func(*resource.Info) error, errs chan<- error) {
	var kind string
	var wg sync.WaitGroup
	for _, info := range infos {
		currentKind := info.Object.GetObjectKind().GroupVersionKind().Kind
		if kind != currentKind {
			wg.Wait()
			kind = currentKind
		}
		wg.Add(1)
		go func(i *resource.Info) {
			errs <- fn(i)
			wg.Done()
		}(info)
	}
}

func (c *Client) applyResource(info *resource.Info) error {
	//obj, err := resource.NewHelper(info.Client, info.Mapping).Create(info.Namespace, true, info.Object)
	//if err != nil {
	//	return err
	//}
	//return info.Refresh(obj, true)

	return c.applier.ApplyOneObject(info)
}

func deleteResource(info *resource.Info) error {
	policy := metav1.DeletePropagationBackground
	opts := &metav1.DeleteOptions{PropagationPolicy: &policy}
	_, err := resource.NewHelper(info.Client, info.Mapping).DeleteWithOptions(info.Namespace, info.Name, opts)
	return err
}
