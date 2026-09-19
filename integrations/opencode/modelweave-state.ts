import { tool } from "@opencode-ai/plugin"

async function run(args: string[], cwd: string) {
  const proc = Bun.spawn(["modelweave", ...args], { cwd, stdout: "pipe", stderr: "pipe" })
  const [stdout, stderr, code] = await Promise.all([
    new Response(proc.stdout).text(),
    new Response(proc.stderr).text(),
    proc.exited,
  ])
  if (code !== 0) throw new Error(stderr.trim() || `modelweave exited ${code}`)
  return stdout.trim()
}

export default tool({
  description: "Inspect or checkpoint ModelWeave cognitive state. Use verify after source changes and commit only when a useful cognition change should persist.",
  args: {
    action: tool.schema.enum(["status", "verify", "commit"]).describe("State operation"),
    message: tool.schema.string().optional().describe("Commit message when action=commit"),
  },
  async execute(args, context) {
    if (args.action === "commit") {
      if (!args.message) throw new Error("message is required for commit")
      return await run(["commit", args.message], context.worktree)
    }
    return await run([args.action], context.worktree)
  },
})
