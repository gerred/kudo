package planexecution

import (
	"context"
	"fmt"
	"log"
	"strconv"

	"k8s.io/apimachinery/pkg/types"

	"github.com/kudobuilder/kudo/pkg/apis/kudo/v1alpha1"
	kudoengine "github.com/kudobuilder/kudo/pkg/engine"
	"github.com/kudobuilder/kudo/pkg/util/health"
	"github.com/pkg/errors"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	apijson "k8s.io/apimachinery/pkg/util/json"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

type activePlan struct {
	Name      string
	State     *v1alpha1.PlanExecutionStatus
	Spec      *v1alpha1.Plan
	Tasks     map[string]v1alpha1.TaskSpec
	Templates map[string]string
	params    map[string]string
}

type planResources struct {
	PhaseResources map[string]phaseResources
}

type phaseResources struct {
	StepResources map[string][]runtime.Object
}

type executionMetadata struct {
	instanceName        string
	instanceNamespace   string
	operatorName        string
	operatorVersionName string
	operatorVersion     string

	planExecutionID string // TODO will be removed when PE CRD is removed

	// the object that will own all the resources created by this execution
	resourcesOwner metav1.Object
}

// executePlan takes a currently active plan and metadata from the underlying operator and executes next "step" in that execution
// the next step could consist of actually executing multiple steps of the plan or just one depending on the execution strategy of the phase (serial/parallel)
// result of running this function is new state of the execution that is returned to the caller (it can either be completed, or still in progress or errored)
// in case of error, error is returned along with the state as well (so that it's possible to report which step caused the error)
// in case of error, method returns both error or fatalError which should indicate unrecoverable error meaning there is no point in retrying that execution
func executePlan(plan *activePlan, metadata *executionMetadata, c client.Client, renderer kubernetesObjectEnhancer) (*v1alpha1.PlanExecutionStatus, error) {
	if isFinished(plan.State.State) {
		log.Printf("PlanExecution: Plan %s for instance %s is completed, nothing to do", plan.Name, metadata.instanceName)
		return plan.State, nil
	}

	// we don't want to modify the original state, and State does not contain any pointer, so shallow copy is enough
	newState := &(*plan.State)

	// render kubernetes resources needed to execute this plan
	planResources, err := prepareKubeResources(plan, metadata, renderer)
	if err != nil {
		newState.State = v1alpha1.PhaseStateError
		return newState, err
	}

	// do a next step in the current plan execution
	allPhasesCompleted := true
	for _, ph := range plan.Spec.Phases {
		currentPhaseState, _ := getPhaseFromStatus(ph.Name, newState)
		if isFinished(currentPhaseState.State) {
			// nothing to do
			log.Printf("PlanExecution: Phase %s on plan %s and instance %s is in state %s, nothing to do", ph.Name, plan.Name, metadata.instanceName, currentPhaseState.State)
			continue
		} else if isInProgress(currentPhaseState.State) {
			currentPhaseState.State = v1alpha1.PhaseStateInProgress
			log.Printf("PlanExecution: Executing phase %s on plan %s and instance %s - it's in progress", ph.Name, plan.Name, metadata.instanceName)

			// we're currently executing this phase
			allStepsHealthy := true
			for _, st := range ph.Steps {
				currentStepState, _ := getStepFromStatus(st.Name, currentPhaseState)
				resources := planResources.PhaseResources[ph.Name].StepResources[st.Name]
				log.Printf("Resources count: %d", len(resources))

				log.Printf("PlanExecution: Executing step %s on plan %s and instance %s - it's in %s state", st.Name, plan.Name, metadata.instanceName, currentStepState.State)
				err := executeStep(st, currentStepState, resources, c)
				if err != nil {
					currentPhaseState.State = v1alpha1.PhaseStateError
					currentStepState.State = v1alpha1.PhaseStateError
					return newState, err
				}

				if !isFinished(currentStepState.State) {
					allStepsHealthy = false
					if ph.Strategy == v1alpha1.Serial {
						// we cannot proceed to the next step
						break
					}
				}
			}

			if allStepsHealthy {
				log.Printf("PlanExecution: All steps on phase %s plan %s and instance %s are healthy", ph.Name, plan.Name, metadata.instanceName)
				currentPhaseState.State = v1alpha1.PhaseStateComplete
			}
		}

		if !isFinished(currentPhaseState.State) {
			// we cannot proceed to the next phase
			allPhasesCompleted = false
			break
		}
	}

	if allPhasesCompleted {
		log.Printf("PlanExecution: All phases on plan %s and instance %s are healthy", plan.Name, metadata.instanceName)
		newState.State = v1alpha1.PhaseStateComplete
	}

	return newState, nil
}

func executeStep(step v1alpha1.Step, state *v1alpha1.StepStatus, resources []runtime.Object, c client.Client) error {
	if isInProgress(state.State) {
		state.State = v1alpha1.PhaseStateInProgress

		// check if step is already healthy
		allHealthy := true
		for _, r := range resources {
			if step.Delete {
				// delete
				log.Printf("PlanExecution: Step %s will delete object %v", step.Name, r)
				err := c.Delete(context.TODO(), r, client.PropagationPolicy(metav1.DeletePropagationForeground))
				if !apierrors.IsNotFound(err) && err != nil {
					return err
				}
			} else {
				// create or update
				log.Printf("Going to create/update %v", r)
				existingResource := r.DeepCopyObject()
				key, _ := client.ObjectKeyFromObject(r)
				err := c.Get(context.TODO(), key, existingResource)
				if apierrors.IsNotFound(err) {
					// create
					err = c.Create(context.TODO(), r)
					if err != nil {
						log.Printf("PlanExecution: error when creating resource in step %v: %v", step.Name, err)
						return err
					}
				} else if err != nil {
					// other than not found error - raise it
					return err
				} else {
					// update
					err := patchExistingObject(r, existingResource, c)
					if err != nil {
						return err
					}
				}

				err = health.IsHealthy(c, r)
				if err != nil {
					allHealthy = false
					log.Printf("PlanExecution: Obj is NOT healthy: %s", prettyPrint(key))
				}
			}
		}

		if allHealthy {
			state.State = v1alpha1.PhaseStateComplete
		}
	}
	return nil
}

// patchExistingObject calls update method on kubernetes client to make sure the current resource reflects what is on server
//
// an obvious optimization here would be to not patch when objects are the same, however that is not easy
// kubernetes native objects might be a problem because we cannot just compare the spec as the spec might have extra fields
// and those extra fields are set by some kubernetes component
// because of that for now we just try to apply the patch every time
func patchExistingObject(newResource runtime.Object, existingResource runtime.Object, c client.Client) error {
	newResourceJSON, _ := apijson.Marshal(newResource)
	key, _ := client.ObjectKeyFromObject(newResource)
	err := c.Patch(context.TODO(), existingResource, client.ConstantPatch(types.StrategicMergePatchType, newResourceJSON))
	if err != nil {
		// Right now applying a Strategic Merge Patch to custom resources does not work. There is
		// certain metadata needed, which when missing, leads to an invalid Content-Type Header and
		// causes the request to fail.
		// ( see https://github.com/kubernetes-sigs/kustomize/issues/742#issuecomment-458650435 )
		//
		// We temporarily solve this by checking for the specific error when a SMP is applied to
		// custom resources and handle it by defaulting to a Merge Patch.
		//
		// The error message for which we check is:
		// 		the body of the request was in an unknown format - accepted media types include:
		//			application/json-patch+json, application/merge-patch+json
		//
		// 		Reason: "UnsupportedMediaType" Code: 415
		if apierrors.IsUnsupportedMediaType(err) {
			err = c.Patch(context.TODO(), newResource, client.ConstantPatch(types.MergePatchType, newResourceJSON))
			if err != nil {
				log.Printf("PlanExecution: Error when applying merge patch to object %v: %v", key, err)
				return err
			}
		} else {
			log.Printf("PlanExecution: Error when applying StrategicMergePatch to object %v: %v", key, err)
			return err
		}
	}
	return nil
}

// prepareKubeResources takes all resources in all tasks for a plan and renders them with the right parameters
// it also takes care of applying KUDO specific conventions to the resources like commond labels
func prepareKubeResources(plan *activePlan, meta *executionMetadata, renderer kubernetesObjectEnhancer) (*planResources, error) {
	configs := make(map[string]interface{})
	configs["OperatorName"] = meta.operatorName
	configs["Name"] = meta.instanceName
	configs["Namespace"] = meta.instanceNamespace
	configs["Params"] = plan.params

	result := &planResources{
		PhaseResources: make(map[string]phaseResources),
	}

	for _, phase := range plan.Spec.Phases {
		phaseState, _ := getPhaseFromStatus(phase.Name, plan.State)
		perStepResources := make(map[string][]runtime.Object)
		result.PhaseResources[phase.Name] = phaseResources{
			StepResources: perStepResources,
		}
		for j, step := range phase.Steps {
			configs["PlanName"] = plan.Name
			configs["PhaseName"] = phase.Name
			configs["StepName"] = step.Name
			configs["StepNumber"] = strconv.FormatInt(int64(j), 10)
			var resources []runtime.Object
			stepState, _ := getStepFromStatus(step.Name, phaseState)

			engine := kudoengine.New()
			for _, t := range step.Tasks {
				if taskSpec, ok := plan.Tasks[t]; ok {
					resourcesAsString := make(map[string]string)

					for _, res := range taskSpec.Resources {
						if resource, ok := plan.Templates[res]; ok {
							templatedYaml, err := engine.Render(resource, configs)
							if err != nil {
								phaseState.State = v1alpha1.PhaseStateError
								stepState.State = v1alpha1.PhaseStateError

								err := errors.Wrapf(err, "error expanding template")
								log.Print(err)
								return nil, fatalError{err: err}
							}
							resourcesAsString[res] = templatedYaml
						} else {
							phaseState.State = v1alpha1.PhaseStateError
							stepState.State = v1alpha1.PhaseStateError

							err := fmt.Errorf("PlanExecution: Error finding resource named %v for operator version %v", res, meta.operatorVersionName)
							log.Print(err)
							return nil, fatalError{err: err}
						}
					}

					resourcesWithConventions, err := renderer.applyConventionsToTemplates(resourcesAsString, metadata{
						InstanceName:    meta.instanceName,
						Namespace:       meta.instanceNamespace,
						OperatorName:    meta.operatorName,
						OperatorVersion: meta.operatorVersion,
						PlanExecution:   meta.planExecutionID,
						PlanName:        plan.Name,
						PhaseName:       phase.Name,
						StepName:        step.Name,
					}, meta.resourcesOwner)

					if err != nil {
						phaseState.State = v1alpha1.PhaseStateError
						stepState.State = v1alpha1.PhaseStateError

						log.Printf("Error creating Kubernetes objects from step %v in phase %v of plan %v: %v", step.Name, phase.Name, meta.planExecutionID, err)
						return nil, err
					}
					resources = append(resources, resourcesWithConventions...)
				} else {
					phaseState.State = v1alpha1.PhaseStateError
					stepState.State = v1alpha1.PhaseStateError

					err := fmt.Errorf("Error finding task named %s for operator version %s", taskSpec, meta.operatorVersionName)
					log.Print(err)
					return nil, fatalError{err: err}
				}
			}

			perStepResources[step.Name] = resources
		}
	}

	return result, nil
}

func getStepFromStatus(stepName string, status *v1alpha1.PhaseStatus) (*v1alpha1.StepStatus, error) {
	for i, p := range status.Steps {
		if p.Name == stepName {
			return &status.Steps[i], nil
		}
	}
	return nil, fmt.Errorf("PlanExecution: Cannot find step %s in plan", stepName)
}

func getPhaseFromStatus(phaseName string, status *v1alpha1.PlanExecutionStatus) (*v1alpha1.PhaseStatus, error) {
	for i, p := range status.Phases {
		if p.Name == phaseName {
			return &status.Phases[i], nil
		}
	}
	return nil, fmt.Errorf("PlanExecution: Cannot find phase %s in plan", phaseName)
}

func isFinished(state v1alpha1.PhaseState) bool {
	return state == v1alpha1.PhaseStateComplete
}

func isInProgress(state v1alpha1.PhaseState) bool {
	return state == v1alpha1.PhaseStateInProgress || state == v1alpha1.PhaseStatePending || state == v1alpha1.PhaseStateError
}
