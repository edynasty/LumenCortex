import readline from 'node:readline/promises';

const ESC = '\u001b[';

export class LumenCortexTui {
  constructor({ agent, sessions, providerLabel = '', parallelRunner, input = process.stdin, output = process.stdout } = {}) {
    if (!agent) throw new Error('agent is required');
    if (!sessions) throw new Error('sessions is required');
    this.agent = agent;
    this.sessions = sessions;
    this.providerLabel = providerLabel;
    this.parallelRunner = parallelRunner;
    this.input = input;
    this.output = output;
    this.currentSessionId = null;
    this.events = [];
    this.lastAnswer = '';
    this.busy = false;
  }

  event(event) {
    this.events.push(formatEvent(event));
    this.events = this.events.slice(-12);
    if (this.output.isTTY) this.render();
  }

  render() {
    if (!this.output.isTTY) return;
    this.output.write(ESC + '2J' + ESC + 'H');
    this.output.write(renderTuiFrame({
      provider: this.providerLabel,
      currentSessionId: this.currentSessionId,
      sessions: this.sessions.list(8),
      events: this.events,
      answer: this.lastAnswer,
      busy: this.busy,
      width: this.output.columns ?? 100
    }));
  }

  async run(options = {}) {
    const terminal = readline.createInterface({ input: this.input, output: this.output, terminal: true });
    this.output.write(ESC + '?1049h');
    try {
      this.render();
      while (true) {
        const line = (await terminal.question('\nlcx> ')).trim();
        if (!line) continue;
        if ([':q', ':quit', ':exit'].includes(line)) break;
        if (line === ':help') {
          this.events.push('commands: :new :sessions :use <id> :parallel <json-file> :quit');
          this.render();
          continue;
        }
        if (line === ':new') {
          this.currentSessionId = null;
          this.lastAnswer = '';
          this.events.push('new session selected');
          this.render();
          continue;
        }
        if (line === ':sessions') {
          this.events.push(...this.sessions.list(8).map((s) => `${s.id} [${s.status}] ${s.goal}`));
          this.events = this.events.slice(-12);
          this.render();
          continue;
        }
        if (line.startsWith(':use ')) {
          const id = line.slice(5).trim();
          this.sessions.load(id);
          this.currentSessionId = id;
          this.events.push(`switched to ${id}`);
          this.render();
          continue;
        }
        if (line.startsWith(':parallel ')) {
          if (!this.parallelRunner) {
            this.events.push('parallel runner is not configured');
            this.render();
            continue;
          }
          const file = line.slice(':parallel '.length).trim();
          const fs = await import('node:fs');
          const tasks = JSON.parse(fs.readFileSync(file, 'utf8'));
          this.busy = true;
          this.render();
          const result = await this.parallelRunner.runTasks(tasks, options.parallelOptions ?? {});
          this.busy = false;
          this.lastAnswer = JSON.stringify(result, null, 2);
          this.render();
          continue;
        }

        this.busy = true;
        this.render();
        try {
          const result = await this.agent.run(line, {
            ...options.agentOptions,
            sessionId: this.currentSessionId ?? undefined
          });
          this.currentSessionId = result.session.id;
          this.lastAnswer = result.final;
        } catch (error) {
          this.currentSessionId = error.sessionId ?? this.currentSessionId;
          this.lastAnswer = `ERROR: ${error.message}`;
        } finally {
          this.busy = false;
          this.render();
        }
      }
    } finally {
      terminal.close();
      this.output.write(ESC + '?1049l');
    }
  }
}

export function renderTuiFrame({ provider = '', currentSessionId, sessions = [], events = [], answer = '', busy = false, width = 100 } = {}) {
  const inner = Math.max(60, Number(width) - 2);
  const top = `LumenCortex TUI  ${provider}  ${busy ? '[RUNNING]' : '[READY]'}`;
  const session = `Session: ${currentSessionId ?? '(new)'}`;
  const sessionLines = sessions.slice(0, 8).map((s) => {
    const marker = s.id === currentSessionId ? '>' : ' ';
    return `${marker} ${s.id} ${String(s.status).padEnd(10)} ${truncate(s.goal, 42)}`;
  });
  const eventLines = events.slice(-10);
  const answerLines = String(answer || '(no answer yet)').split(/\r?\n/).slice(-10);

  return [
    boxLine(top, inner),
    boxLine(session, inner),
    divider(inner),
    boxLine('Recent sessions', inner),
    ...sessionLines.map((x) => boxLine(x, inner)),
    divider(inner),
    boxLine('Agent events', inner),
    ...eventLines.map((x) => boxLine(x, inner)),
    divider(inner),
    boxLine('Last answer', inner),
    ...answerLines.map((x) => boxLine(x, inner)),
    divider(inner),
    boxLine(':new  :sessions  :use <id>  :parallel <tasks.json>  :help  :quit', inner)
  ].join('\n') + '\n';
}

function formatEvent(event) {
  if (event.type === 'llm.request') return `step ${event.step} → ${event.model}`;
  if (event.type === 'tool.start') return `tool → ${event.name}`;
  if (event.type === 'tool.end') return `tool ← ${event.name} ${event.ok ? 'ok' : 'error'}`;
  if (event.type === 'context.move') return `light ${event.selectedNodes} nodes / ${event.contextTokens}t`;
  if (event.type === 'context.promote') return `promote → ${event.abstractionId}`;
  if (event.type === 'session.complete') return `completed at step ${event.step}`;
  if (event.type === 'llm.retry') return `retry ${event.attempt}: ${event.error}`;
  return event.type;
}

function boxLine(text, width) {
  return '│ ' + truncate(text, width - 4).padEnd(width - 3) + '│';
}

function divider(width) {
  return '├' + '─'.repeat(width - 2) + '┤';
}

function truncate(value, max) {
  const text = String(value ?? '');
  return text.length <= max ? text : text.slice(0, Math.max(0, max - 1)) + '…';
}

export { LumenCortexTui as ModelWeaveTui };
