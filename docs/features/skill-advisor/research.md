# Skill advisor: research

Working notes for a feature that suggests edits to existing skills and new skills to create. Delete this folder once the feature ships, and move anything durable into the README.

## What the transcripts show (Codex, March–October 2026)

The data covers 4,500 transcripts: 3,287 main sessions with prompts and 1,068 subagent sessions.

**Skills with a clear trigger work well.** The table compares sessions that did the matching action with the time the skill was read. It counts only sessions where the skill was listed, from September onward.

| Action in session | Skill            | Read before | Read after | Never |
|-------------------|------------------|------------:|-----------:|------:|
| `git commit`      | commiting        | 200         | 8          | 11    |
| `glab`            | company-gitlab   | 35          | 2          | 2     |
| Plane MCP call    | plane-workflow   | 144         | 2          | 5     |

**Tool signals alone are too noisy to find misses.**
- 30% of sessions that ran Playwright did not read playwright-cli. Most of those used the project-local `website-inspection` skill or live-browser instead, which is correct.
- 51% of sessions that touched test files did not read designing-software-tests. Most were only running tests.

Telling a real miss from a false alarm means reading the prompt, so this needs a model.

**The strongest signal is the user correcting the agent.** These are rare, but each is clear evidence. Examples:
- "wtf? why are you not using the teamwork mcp? do not use the live browser". The agent said live-browser's description made the browser route look relevant. The description has since gained "Do not use for accessing teamwork, gitlab…", the same fix the advisor should suggest.
- "and did you read the commiting skill BEFORE or AFTER commiting?"
- "Because my skill instructions require me to reload a matching skill at the start of every turn ??? which instructions?"

**Recurring instructions are paraphrased, so string matching doesn't find them.** Exact repeated sentences across `~/.codex/history.jsonl` are mostly pasted logs and lint output. The real repeats were found by the model pilot (below), not by n-grams. An example is "validate reviewer findings before accepting them as gospel", which the user retyped by hand.

**The Codex system prompt discourages implicit use.** It says: "Do not use a skill based solely on keywords, superficial relevance…". So a description must make the fit obvious, not just mention the right words. Skills with `allow_implicit_invocation: false` are not listed at all.

## Pragmatic practice (external)

- The description is the trigger. Agents pick the right skill when they activate one; the failure is not activating. Put trigger terms first: Codex shortens descriptions when the list exceeds its budget (2% of context, or 8,000 characters). ([OpenAI][codex], [Spence][spence])
- Default activation on generic wording is about 50% in Claude Code. Distinctive keywords trigger almost always; indirect phrasing almost never. A forced yes/no evaluation hook reached 100% with no false positives. ([Spence][spence], [Seleznov][seleznov])
- Add negative triggers ("Do not use for…") where skills compete. ([mgechev][mgechev])
- Keep the body under 500 lines with references one level deep. Prefer scripts for fragile, deterministic steps. Build evaluations before writing much. ([Anthropic][anthropic])
- Improve skills from observed use. One agent writes the skill, another uses it, and you feed what went wrong back. Watch for files never read and files always read. ([Anthropic][anthropic])
- Transcript-to-proposal tools already exist and share one pipeline: transcript → evidence → proposed edit → human approval → check that it fired. `reflect-skills` clusters failures across weeks of Claude Code and Codex logs, keeps unproven patterns in an observations file until they recur, and later prunes changes that never fired. ([reflect][reflect], [agent-retro][retro])

## Options

| Option | What it sees | Verdict |
|--------|--------------|---------|
| Single model call: skill text + usage numbers | The skill, counts | Cheap but blind. It can lint wording, which the linter already does. Not worth it. |
| `codex exec` agent over transcripts | Everything, by choice | Finds real misses, misfires and repeated workflows. Worth it. |
| Agent that edits skills itself | Everything | No. Proposals must be reviewed, and the skill files are guarded anyway. |

## Pilot

The pilot ran `codex exec -m gpt-6.1-sol -c model_reasoning_effort=medium -s read-only --ephemeral --output-schema …` over a digest of the last 7 days: 121 sessions with prompts, skills read, tool signals and the transcript path.

- **Cost:** 272 s, 2.09 M input tokens (94% cached), 8.9 k output tokens, 25 shell commands. That is a noticeable but acceptable share of a subscription quota for a weekly run.
- **Quality:**
  - It found 3 timing misses (designing-software-tests twice, dotfiles once), each with transcript line numbers.
  - It found 3 misfires, including commiting loaded by a branch-pruning runner and its stop rule blocking a stash. It also caught the "cryptic descriptions" pushback on plane-workflow and correctly blamed the batch, not the skill.
  - It proposed concrete edits to 4 skills and 2 new skills.
  - About two-thirds of this is useful. One new-skill suggestion (`safe-worktree-cleanup`) restates a scheduled runner's prompt that already exists, so it adds nothing. The advisor needs to know the user's automations.
- `--ephemeral` left no transcript. This matters: otherwise the advisor's own reads of every SKILL.md would show up as usage.

## Proposed shape

1. **Go builds the digest.** It's deterministic, cheap and already half there in `internal/usage`. Per session it holds the prompts, skills read with their order against the first matching action, explicit invocations, and correction-shaped follow-ups such as "why didn't you", "use the X skill" or "did you read". The digest is what makes one run affordable: the agent opens transcripts only to confirm.
2. **Codex investigates.** Use `codex exec --ephemeral -s read-only --output-schema`. The prompt holds the know-how above and is shipped in the binary. It covers the period since the last run.
3. **Skillboard shows proposals, not changes.** Each one has evidence links and a diff of the description or body, plus Apply and Dismiss. Dismissed or unproven ones are remembered, so the next run doesn't repeat them and only promotes patterns that recur, as `reflect` does with its observations file.
4. **Close the loop with the trigger tester** (already on the v2 list). Before applying a description change, run a few prompts drawn from the missed sessions through `codex exec` with old and new descriptions, and check whether SKILL.md gets read.

New-skill suggestions come from the same run (the `new_skills` field). They should require at least 3 sessions and no existing skill or automation covering them.

[anthropic]: https://platform.claude.com/docs/en/agents-and-tools/agent-skills/best-practices
[codex]: https://learn.chatgpt.com/docs/build-skills
[spence]: https://scottspence.com/posts/measuring-claude-code-skill-activation-with-sandboxed-evals
[seleznov]: https://medium.com/@ivan.seleznov1/why-claude-code-skills-dont-activate-and-how-to-fix-it-86f679409af1
[mgechev]: https://github.com/mgechev/skills-best-practices
[reflect]: https://github.com/ckorhonen/reflect
[retro]: https://github.com/giannimassi/agent-retro
