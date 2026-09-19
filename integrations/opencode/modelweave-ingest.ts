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
  description: "Incrementally ingest the current repository into ModelWeave after meaningful code changes.",
  args: {},
  async execute(_args, context) {
    return await run(["ingest", context.worktree], context.worktree)
  },
})
