// Command embedded shows Fliable as a library: the whole BPM platform
// inside your own Go program — no server, no database, no JVM.
//
//	go run ./examples/embedded
package main

import (
	"fmt"
	"log"

	"github.com/olbboy/fliable/engine"
	"github.com/olbboy/fliable/store"
)

const processXML = `<?xml version="1.0" encoding="UTF-8"?>
<definitions xmlns="http://www.omg.org/spec/BPMN/20100524/MODEL"
             xmlns:xsi="http://www.w3.org/2001/XMLSchema-instance"
             xmlns:fliable="https://fliable.dev/schema/1.0"
             targetNamespace="https://fliable.dev/examples">
  <process id="onboarding" name="Employee onboarding" isExecutable="true">
    <startEvent id="start"/>
    <sequenceFlow id="f1" sourceRef="start" targetRef="createAccounts"/>

    <serviceTask id="createAccounts" name="Create accounts" fliable:type="accounts.create"/>
    <sequenceFlow id="f2" sourceRef="createAccounts" targetRef="assignBuddy"/>

    <userTask id="assignBuddy" name="Assign onboarding buddy" fliable:candidateGroups="managers"/>
    <sequenceFlow id="f3" sourceRef="assignBuddy" targetRef="notifyTeam"/>

    <serviceTask id="notifyTeam" name="Notify the team" fliable:type="team.notify"/>
    <sequenceFlow id="f4" sourceRef="notifyTeam" targetRef="end"/>
    <endEvent id="end"/>
  </process>
</definitions>`

func main() {
	// 1. Engine on the in-memory store (use store.OpenJournal for
	//    durability).
	eng := engine.New(store.NewMemory())

	// 2. Service tasks are plain Go functions — type-safe, testable, fast.
	eng.RegisterHandler("accounts.create", func(ctx engine.Context) (map[string]any, error) {
		name := ctx.Variables["name"]
		fmt.Printf("  [service] creating accounts for %v\n", name)
		return map[string]any{"email": fmt.Sprintf("%v@example.com", name)}, nil
	})
	eng.RegisterHandler("team.notify", func(ctx engine.Context) (map[string]any, error) {
		fmt.Printf("  [service] welcome email sent to %v, buddy: %v\n",
			ctx.Variables["email"], ctx.Variables["buddy"])
		return nil, nil
	})

	// 3. Watch every engine event live (audit, websockets, metrics...).
	eng.OnEvent(func(ev *store.HistoryEvent) {
		fmt.Printf("  [event] %-22s %s\n", ev.Type, ev.ElementID)
	})

	// 4. Deploy and start.
	if _, err := eng.Deploy([]byte(processXML), ""); err != nil {
		log.Fatal(err)
	}
	inst, err := eng.StartInstance("onboarding", "emp-42", map[string]any{"name": "dat"})
	if err != nil {
		log.Fatal(err)
	}
	fmt.Printf("instance %s is %s (waiting on the human step)\n", inst.ID, inst.State)

	// 5. Work the user task like a task-list app would.
	tasks, _ := eng.ListTasks(store.TaskFilter{CandidateGroup: "managers", State: store.TaskCreated})
	fmt.Printf("open task: %q\n", tasks[0].Name)
	if err := eng.CompleteTask(tasks[0].ID, map[string]any{"buddy": "alice"}, "manager-bob"); err != nil {
		log.Fatal(err)
	}

	final, _ := eng.GetInstance(inst.ID)
	fmt.Printf("instance finished: %s in %s\n", final.State, final.EndedAt.Sub(final.StartedAt))
}
