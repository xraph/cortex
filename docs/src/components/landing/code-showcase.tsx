"use client";

import { motion } from "framer-motion";
import { CodeBlock } from "./code-block";
import { SectionHeader } from "./section-header";

const setupCode = `package main

import (
  "context"
  "log/slog"

  "github.com/xraph/cortex/engine"
  "github.com/xraph/cortex/agent"
  pgstore "github.com/xraph/cortex/store/postgres"
)

func main() {
  ctx := context.Background()

  eng, _ := engine.New(
    engine.WithStore(pgstore.New(bunDB)),
    engine.WithLogger(slog.Default()),
  )

  // Create an agent with persona and skills
  eng.CreateAgent(ctx, &agent.Config{
    Name:          "support-agent",
    Model:         "gpt-4o",
    ReasoningLoop: "react",
    PersonaRef:    "helpful-agent",
    InlineSkills:  []string{"customer-support"},
    MaxSteps:      15,
  })
}`;

const executeCode = `package main

import (
  "context"
  "fmt"

  "github.com/xraph/cortex"
  "github.com/xraph/cortex/engine"
)

func runAgent(
  eng *engine.Engine,
  ctx context.Context,
) {
  ctx = cortex.WithTenant(ctx, "acme-corp")

  // Run the agent synchronously
  result, _ := eng.RunAgent(ctx,
    "support-agent",
    "I want to return order #12345",
  )

  fmt.Println(result.Output)
  // "I'd be happy to help with your return..."
  fmt.Printf("Steps: %d, Tokens: %d\\n",
    result.StepCount, result.TokensUsed)
}`;

export function CodeShowcase() {
  return (
    <section className="relative w-full py-20 sm:py-28">
      <div className="container max-w-(--fd-layout-width) mx-auto px-4 sm:px-6">
        <SectionHeader
          badge="Developer Experience"
          title="Simple API. Intelligent agents."
          description="Create an agent and run it in under 30 lines. Cortex handles persona resolution, reasoning loops, and tool orchestration."
        />

        <div className="mt-14 grid grid-cols-1 lg:grid-cols-2 gap-6">
          {/* Agent setup side */}
          <motion.div
            initial={{ opacity: 0, x: -20 }}
            whileInView={{ opacity: 1, x: 0 }}
            viewport={{ once: true }}
            transition={{ duration: 0.5, delay: 0.1 }}
          >
            <div className="mb-3 flex items-center gap-2">
              <div className="size-2 rounded-full bg-violet-500" />
              <span className="text-xs font-medium text-fd-muted-foreground uppercase tracking-wider">
                Agent Setup
              </span>
            </div>
            <CodeBlock code={setupCode} filename="main.go" />
          </motion.div>

          {/* Agent execution side */}
          <motion.div
            initial={{ opacity: 0, x: 20 }}
            whileInView={{ opacity: 1, x: 0 }}
            viewport={{ once: true }}
            transition={{ duration: 0.5, delay: 0.2 }}
          >
            <div className="mb-3 flex items-center gap-2">
              <div className="size-2 rounded-full bg-green-500" />
              <span className="text-xs font-medium text-fd-muted-foreground uppercase tracking-wider">
                Agent Execution
              </span>
            </div>
            <CodeBlock code={executeCode} filename="run.go" />
          </motion.div>
        </div>
      </div>
    </section>
  );
}
