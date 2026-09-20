import { tool } from "@opencode-ai/plugin"

async function run(args: string[], cwd: string) {
  const proc = Bun.spawn(["lcx", ...args], { cwd, stdout: "pipe", stderr: "pipe" })
  const [stdout, stderr, code] = await Promise.all([
    new Response(proc.stdout).text(),
    new Response(proc.stderr).text(),
    proc.exited,
  ])
  if (code !== 0) throw new Error(stderr.trim() || `lcx exited ${code}`)
  return stdout.trim()
}

export default tool({
  description: "Illuminate the smallest relevant LumenCortex context subgraph for the current coding question. Prefer this before broad repository exploration.",
  args: {
    goal: tool.schema.string().describe("Concrete question or coding goal"),
    budget: tool.schema.number().optional().describe("Estimated context token budget, default 32000"),
    multi: tool.schema.boolean().optional().describe("Also run explore/contrarian/anomaly lights"),
  },
  async execute(args, context) {
    const cmd = ["light", args.goal, "--budget", String(args.budget ?? 32000), "--json"]
    if (args.multi) cmd.push("--multi")
    return await run(cmd, context.worktree)
  },
})
