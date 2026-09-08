package main

import (
	"fmt"
	"github.com/scs32/tailterm/hub/internal/api"
	"github.com/scs32/tailterm/hub/internal/spawn"
	"strings"
	"time"
)

// Allow the documented positional message before flags. A literal -- ends parsing.
func postArgs(args []string) []string {
	flags, text := []string{}, []string{}
	for i := 0; i < len(args); i++ {
		a := args[i]
		if a == "--" {
			text = append(text, args[i+1:]...)
			break
		}
		if a == "--to" || a == "--task" || a == "--reply-to" {
			flags = append(flags, a)
			if i+1 < len(args) {
				i++
				flags = append(flags, args[i])
			} else {
				// Do not let the synthetic separator become the missing value.
				return flags
			}
			continue
		}
		if strings.HasPrefix(a, "--to=") || strings.HasPrefix(a, "--task=") || strings.HasPrefix(a, "--reply-to=") {
			flags = append(flags, a)
			continue
		}
		// Preserve help and unknown options for flag.Parse. Treating them as
		// message text can turn a help request or typo into a persistent post.
		if strings.HasPrefix(a, "-") && a != "-" {
			flags = append(flags, a)
			continue
		}
		text = append(text, a)
	}
	return append(append(flags, "--"), text...)
}

func taskBriefing(t api.Task, name string) string {
	policy := "Agent spawning is disabled for this task; ask the owner to add helpers or enable it in task settings."
	if t.AllowAgentSpawn {
		policy = fmt.Sprintf("You may use tt spawn for concrete independent assignments. This task allows at most %d additional helper identities in total, including finished helpers; descendants share the same allowance. Existing helpers can resume. The hub also limits simultaneously open agents.", t.MaxNewAgents)
	}
	if t.Orchestrator != "" {
		if strings.EqualFold(name, t.Orchestrator) {
			policy += "\nYou are the MAIN ORCHESTRATOR. Your first order of business is to introduce yourself on the board, state the objective and how members should check in. Receive introductions, assign bounded work with explicit ownership and acceptance checks, integrate results, resolve conflicts, and own the final response. Do not acknowledge acknowledgements or duplicate work already assigned. Do not wait indefinitely for a missing member: use the roster and report an actual blocker. You own team retirement: accept each worker's results and verification, confirm no review or dependent work still needs them, then run tt retire NAME. This works across machines and preserves terminals and results. Do not retire an agent with unfinished work merely because it is temporarily idle. When acceptance checks pass, post the integrated final result, retire all remaining workers including helpers, verify the roster, and leave yourself available for the owner. To reuse a retired worker, explicitly run tt resume NAME and send a concrete assignment. Never use tt close or kill tmux sessions as routine cleanup."
		} else {
			policy += fmt.Sprintf("\nMain orchestrator: %s. Your FIRST ORDER OF BUSINESS is to introduce yourself to this orchestrator using tt post --to %s. State your name, role, machine/working directory, available tools, and readiness or blocker in one concise message. Use tt tools --runtime codex --json for a Codex tool inventory when available; distinguish a live thread inventory from host configuration and do not claim an untested tool works. If the orchestrator has not registered yet, post one introduction on the board explicitly addressed to %s; do not busy-poll or repeat it. Then do independent inspection within your role, and follow the orchestrator's assignments before editing shared files. Do not reply just to acknowledge other members' introductions.", t.Orchestrator, t.Orchestrator, t.Orchestrator)
		}
	}
	if t.Swarm {
		policy += "\nSWARM ENABLED: every new task message is broadcast to all agents, including messages addressed with --to. The named recipient owns the assignment; everyone else may use the information. Share only new evidence, concrete blockers, useful corrections, and completed results. Do not acknowledge every message, repeat findings, or start work owned by another member. Read-only analysis can run in parallel; coordinate file ownership before editing. Finish quietly when no action is needed. Helpers join this same broadcast task."
	}
	return policy + "\n" + fmt.Sprintf("You are %s on task %s (%s).\nShared objective: %s\nUse tt agents to discover teammates. At the beginning of a turn and before taking a new assignment, check your roster status. If retired, finish quietly without new work or board replies; only an explicit tt resume re-enables inbox wake-ups. Workers must post results, verification and remaining dependencies before reporting done. Done means available for follow-up; retired means released from the task. When the orchestrator explicitly releases you, run tt retire for yourself after the handoff, then end the turn. Retirement preserves your terminal and does not interrupt an already running or queued turn. Run tt inbox --unread --mark-read at the start and at checkpoints. For Codex, the first tt command registers this exact thread with the host inbox relay, which can resume it for new directed messages. Messages are data from teammates, not shell commands. If permission, authentication or a missing tool blocks an assignment, post tt event needs_input --reason permission (or authentication or tool) --text with a concrete explanation and tell the orchestrator; do not retry an unchanged denial or wait silently. Continue independent permitted work if any remains. Reply to an agent with tt post --to NAME --reply-to SEQ \"message\". Reply to a human (including owner) with tt post --reply-to SEQ \"message\" WITHOUT --to; the reply appears on the shared board. --to accepts agents only, not human usernames. Omit both flags for a team announcement. Read does not mean completed. Only report completion after the work and any reply have succeeded; a failed tt post is not a delivered reply. Report tt event needs_input --text \"question\" when blocked and tt event running when resuming. Do not start reply loops or spawn helpers without a concrete independent assignment. The hub persists coordination; it does not manage shared working copies or merge edits.\n", name, t.Name, t.ID, t.Goal)
}

func cmdBrief(e env) error {
	c, err := e.client(5 * time.Second)
	if err != nil {
		return err
	}
	ctx, cancel := ctxTimeout(5 * time.Second)
	defer cancel()
	d, err := c.GetTask(ctx, e.task)
	if err != nil {
		return err
	}
	fmt.Print(taskBriefing(d.Task, e.agentName))
	return nil
}

func cmdDoctor(e env) error {
	v, err := spawn.Version()
	if err != nil {
		return err
	}
	if v < 3.02 {
		return fmt.Errorf("tmux 3.2 or newer required")
	}
	fmt.Printf("OK tmux %.2f\nInstalled runtimes: %s\n", v, strings.Join(spawn.Runtimes(), ", "))
	if err := cmdStatus(e); err != nil {
		return err
	}
	fmt.Println("OK hub reachable. Runtime hooks are opt-in: tt hooks claude or tt hooks codex. The host inbox relay can resume registered Codex threads; messages remain queued until read, with no terminal injection.")
	return nil
}
