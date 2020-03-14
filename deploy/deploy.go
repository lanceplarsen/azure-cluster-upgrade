package deploy

import (
	"context"
	"math"
	"os"
	"sync"

	"github.com/spf13/cobra"

	"github.com/Azure/azure-sdk-for-go/services/compute/mgmt/2019-07-01/compute"
	"github.com/Azure/go-autorest/autorest"
	"github.com/Azure/go-autorest/autorest/azure/auth"
	"github.com/Azure/go-autorest/autorest/to"
	log "github.com/sirupsen/logrus"
)

var (
	ctx = context.Background()
)

type azureSession struct {
	ResourceGroupName string
	ScaleSetName      string
	SubscriptionID    string
	Authorizer        *autorest.Authorizer
}

func (s *azureSession) getVMSSClient() compute.VirtualMachineScaleSetsClient {
	client := compute.NewVirtualMachineScaleSetsClient(s.SubscriptionID)
	client.Authorizer = *s.Authorizer
	return client
}

func (s *azureSession) getVMSSVMClient() compute.VirtualMachineScaleSetVMsClient {
	client := compute.NewVirtualMachineScaleSetVMsClient(s.SubscriptionID)
	client.Authorizer = *s.Authorizer
	return client
}

func (s *azureSession) setVMProtection(protect bool) ([]compute.VirtualMachineScaleSetVMsUpdateFuture, error) {
	var futures []compute.VirtualMachineScaleSetVMsUpdateFuture
	var filter string

	client := s.getVMSSVMClient()

	if protect {
		filter = "properties/latestModelApplied eq true"
		log.Info("Applying scale-in protection to new instances...")
	} else {
		// Leave this defaulted to an empty string for now
		// This will un-protect ALL members of the VMSS upon completion
		// filter = "properties/latestModelApplied eq false"
		log.Info("Removing scale-in protection from Scale Set instances...")
	}

	for vms, err := client.ListComplete(ctx, s.ResourceGroupName, s.ScaleSetName, filter, "", ""); vms.NotDone(); err = vms.Next() {
		if err != nil {
			return futures, err
		}

		vm := vms.Value()

		vm.ProtectionPolicy = &compute.VirtualMachineScaleSetVMProtectionPolicy{
			ProtectFromScaleIn:         &protect,
			ProtectFromScaleSetActions: to.BoolPtr(false),
		}

		future, err := client.Update(
			context.Background(),
			s.ResourceGroupName,
			s.ScaleSetName,
			*vm.InstanceID,
			vm,
		)
		if err != nil {
			return futures, err
		}

		futures = append(futures, future)
	}

	return futures, nil
}

func (s *azureSession) awaitVMFutures(futures []compute.VirtualMachineScaleSetVMsUpdateFuture) error {
	var wg sync.WaitGroup

	for _, future := range futures {
		client := s.getVMSSVMClient()

		wg.Add(1)
		go func(ctx context.Context, client compute.VirtualMachineScaleSetVMsClient, future compute.VirtualMachineScaleSetVMsUpdateFuture) {
			defer wg.Done()

			err := future.WaitForCompletionRef(ctx, client.Client)
			if err != nil {
				log.Fatal(err)
				return
			}

			res, err := future.Result(client)
			if err != nil {
				log.Fatal(err)
				return
			}

			log.Infof("Modified VM: %s", *res.Name)
		}(ctx, client, future)
	}

	wg.Wait()
	return nil
}

func (s *azureSession) scaleVMSSByFactor(factor float64) error {
	client := s.getVMSSClient()

	scaleSet, err := client.Get(ctx, s.ResourceGroupName, s.ScaleSetName)
	if err != nil {
		return err
	}

	// Ick
	newCapacity := int64(math.Floor(float64(*scaleSet.Sku.Capacity) * factor))

	log.Infof("Scaling VMSS %s to %d instances...", *scaleSet.Name, newCapacity)

	future, err := client.Update(
		ctx,
		s.ResourceGroupName,
		s.ScaleSetName,
		compute.VirtualMachineScaleSetUpdate{
			Sku: &compute.Sku{
				Name:     scaleSet.Sku.Name,
				Tier:     scaleSet.Sku.Tier,
				Capacity: &newCapacity,
			},
		},
	)
	if err != nil {
		return err
	}

	err = future.WaitForCompletionRef(ctx, client.Client)
	if err != nil {
		return err
	}

	return nil
}

func newSession(subscription string, rg string, scaleSet string) (*azureSession, error) {
	authorizer, err := auth.NewAuthorizerFromCLI()
	if err != nil {
		return &azureSession{}, err
	}

	return &azureSession{
		SubscriptionID:    subscription,
		ResourceGroupName: rg,
		ScaleSetName:      scaleSet,
		Authorizer:        &authorizer,
	}, nil
}

// Run initializes a session and executes the upgrade operation
func Run(cmd *cobra.Command, args []string) {
	log.Info("Initializing Cluster Blue/Green Upgrade")

	sess, err := newSession(
		cmd.Flags().Lookup("subscription-id").Value.String(),
		cmd.Flags().Lookup("resource-group").Value.String(),
		cmd.Flags().Lookup("vm-scale-set").Value.String(),
	)
	if err != nil {
		log.Fatal(err)
		os.Exit(1)
	}

	if err = sess.scaleVMSSByFactor(2); err != nil {
		log.Fatal(err)
		os.Exit(1)
	}

	log.Info("Waiting for new instances to reach Running state...")

	// Protect newly-created instances
	scaleOutFutures, err := sess.setVMProtection(true)
	if err != nil {
		log.Fatal(err)
		os.Exit(1)
	}

	if err = sess.awaitVMFutures(scaleOutFutures); err != nil {
		log.Fatal(err)
		os.Exit(1)
	}

	// Halve VMSS Capacity
	if err = sess.scaleVMSSByFactor(0.5); err != nil {
		log.Fatal(err)
		os.Exit(1)
	}

	// Un-protect instances
	scaleInFutures, err := sess.setVMProtection(false)
	if err != nil {
		log.Fatal(err)
		os.Exit(1)
	}

	if err = sess.awaitVMFutures(scaleInFutures); err != nil {
		log.Fatal(err)
		os.Exit(1)
	}
}