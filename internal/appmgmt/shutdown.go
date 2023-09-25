package appmgmt

import (
	"context"
	"sync/atomic"
	"time"

	"github.com/go-logr/logr"
	apiv1 "github.com/microsoft/usvc-apiserver/api/v1"
	"github.com/microsoft/usvc-apiserver/pkg/dcpclient"

	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/dynamic/dynamicinformer"
	"k8s.io/client-go/tools/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/config"
)

func ShutdownApp(ctx context.Context, logger logr.Logger) error {
	shutdownCtx, shutdownCtxCancel := context.WithCancel(ctx)
	defer shutdownCtxCancel()

	if len(apiv1.CleanupResources) <= 0 {
		logger.Info("No resources to delete")
		return nil
	}

	dcpclient, err := dcpclient.New(shutdownCtx, 5*time.Second)
	if err != nil {
		logger.Error(err, "could not get dcpclient")
		return err
	}

	clusterConfig, err := config.GetConfig()
	if err != nil {
		logger.Error(err, "could not get config")
		return err
	}

	dynamicClient, err := dynamic.NewForConfig(clusterConfig)
	if err != nil {
		logger.Error(err, "could not get client")
		return err
	}

	factory := dynamicinformer.NewDynamicSharedInformerFactory(dynamicClient, 5*time.Second)
	defer factory.Shutdown()

	currentWeight := apiv1.CleanupResources[0].Weight
	batchIndex := 0
	resourceBatches := [][]*apiv1.WeightedResource{}
	for _, resource := range apiv1.CleanupResources {
		// If the context is cancelled, stop the informers
		if ctx.Err() != nil {
			logger.Info("Context cancelled, stopping shutdown")
			return ctx.Err()
		}

		if resource.Weight != currentWeight {
			batchIndex += 1
		}

		if batchIndex >= len(resourceBatches) {
			resourceBatches = append(resourceBatches, []*apiv1.WeightedResource{})
		}

		resourceBatches[batchIndex] = append(resourceBatches[batchIndex], resource)
		logger.Info("Adding resource to current batch", "resource", resource.Object.GetGroupVersionResource(), "length", len(resourceBatches[batchIndex]))
	}

	logger.Info("Resource batches generated", "batches", len(resourceBatches))

	for _, resourceBatch := range resourceBatches {
		logger.Info("Cleaning up resource batch", "weight", resourceBatch[0].Weight, "length", len(resourceBatch))
		if err := cleanupResourceBatch(shutdownCtx, dcpclient, factory, resourceBatch, logger); err != nil {
			return err
		}
	}

	return nil
}

func cleanupResourceBatch(
	ctx context.Context,
	dcpclient client.Client,
	factory dynamicinformer.DynamicSharedInformerFactory,
	batch []*apiv1.WeightedResource,
	logger logr.Logger,
) error {
	cleanupCtx, cleanupCtxCancel := context.WithCancel(ctx)
	defer cleanupCtxCancel()

	initialResourceCounts := &atomic.Int32{}
	totalResourceCounts := &atomic.Int32{}

	for _, resource := range batch {
		gvr := resource.Object.GetGroupVersionResource()
		informer := factory.ForResource(gvr)

		logger.Info("Shutting down resource", "resource", gvr)
		if _, err := informer.Informer().AddEventHandler(cache.ResourceEventHandlerFuncs{
			AddFunc: func(obj interface{}) {
				clientObj := obj.(client.Object)
				initialResourceCounts.Add(1)
				totalResources := totalResourceCounts.Add(1)
				logger.Info("Deleting resource", "resource", gvr, "total", totalResources)
				if err := dcpclient.Delete(ctx, clientObj); err != nil {
					logger.Error(err, "could not delete resource", "resource", clientObj)
				}
			},
			DeleteFunc: func(obj interface{}) {
				totalResources := totalResourceCounts.Add(-1)
				logger.Info("Resource removed", "resource", gvr, "total", totalResources)

				if totalResources <= 0 {
					cleanupCtxCancel()
				}
			},
		}); err != nil {
			return err
		}
	}

	factory.Start(ctx.Done())
	_ = factory.WaitForCacheSync(ctx.Done())

	count := initialResourceCounts.Load()
	logger.Info("Waiting for resource batch to be deleted", "initialCount", count)
	if count <= 0 {
		cleanupCtxCancel()
	}

	// Wait for the current batch of resources to be deleted before starting the next batch
	<-cleanupCtx.Done()

	return nil
}
