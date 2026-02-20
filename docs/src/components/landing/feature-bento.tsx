"use client";

import { motion } from "framer-motion";
import { cn } from "@/lib/cn";
import { CodeBlock } from "./code-block";
import { SectionHeader } from "./section-header";

interface FeatureCard {
  title: string;
  description: string;
  icon: React.ReactNode;
  code: string;
  filename: string;
  colSpan?: number;
}

const features: FeatureCard[] = [
  {
    title: "Agent Orchestration",
    description:
      "Create agents with configurable reasoning loops, tool access, and persona references. Cortex handles the full lifecycle from input to intelligent action.",
    icon: (
      <svg
        className="size-5"
        viewBox="0 0 24 24"
        fill="none"
        stroke="currentColor"
        strokeWidth="1.5"
        strokeLinecap="round"
        strokeLinejoin="round"
        aria-hidden="true"
      >
        <circle cx="12" cy="8" r="4" />
        <path d="M5 20v-1a7 7 0 0114 0v1" />
        <path d="M12 12v4" />
      </svg>
    ),
    code: `agent, err := eng.CreateAgent(ctx,
  &agent.Config{
    Name:          "support-agent",
    Model:         "gpt-4o",
    ReasoningLoop: "react",
    PersonaRef:    "helpful-agent",
    MaxSteps:      15,
  })
// agent.ID = agt_01h455...`,
    filename: "agent.go",
  },
  {
    title: "Persona Composition",
    description:
      "Compose personas from skills, traits, and behaviors to create human-emulating agents with distinct personalities and capabilities.",
    icon: (
      <svg
        className="size-5"
        viewBox="0 0 24 24"
        fill="none"
        stroke="currentColor"
        strokeWidth="1.5"
        strokeLinecap="round"
        strokeLinejoin="round"
        aria-hidden="true"
      >
        <path d="M16 21v-2a4 4 0 00-4-4H6a4 4 0 00-4 4v2" />
        <circle cx="9" cy="7" r="4" />
        <path d="M22 21v-2a4 4 0 00-3-3.87" />
        <path d="M16 3.13a4 4 0 010 7.75" />
      </svg>
    ),
    code: `p, err := eng.CreatePersona(ctx,
  &persona.Persona{
    Name:     "helpful-agent",
    Identity: "I am a support specialist.",
    Skills:   []persona.SkillRef{
      {SkillName: "customer-support",
       Proficiency: "expert"},
    },
  })`,
    filename: "persona.go",
  },
  {
    title: "Multi-Tenant Isolation",
    description:
      "Every agent, run, skill, and persona is scoped to a tenant via context. Cross-tenant access is structurally impossible.",
    icon: (
      <svg
        className="size-5"
        viewBox="0 0 24 24"
        fill="none"
        stroke="currentColor"
        strokeWidth="1.5"
        strokeLinecap="round"
        strokeLinejoin="round"
        aria-hidden="true"
      >
        <path d="M17 21v-2a4 4 0 00-4-4H5a4 4 0 00-4 4v2" />
        <circle cx="9" cy="7" r="4" />
        <path d="M23 21v-2a4 4 0 00-3-3.87M16 3.13a4 4 0 010 7.75" />
      </svg>
    ),
    code: `ctx = cortex.WithTenant(ctx, "acme-corp")
ctx = cortex.WithApp(ctx, "support-app")

// All agents, runs, and resources are
// automatically scoped to acme-corp`,
    filename: "scope.go",
  },
  {
    title: "Pluggable Stores",
    description:
      "Start with in-memory for development, swap to PostgreSQL for production. Every domain entity is a Go interface — 50 methods across 8 sub-interfaces.",
    icon: (
      <svg
        className="size-5"
        viewBox="0 0 24 24"
        fill="none"
        stroke="currentColor"
        strokeWidth="1.5"
        strokeLinecap="round"
        strokeLinejoin="round"
        aria-hidden="true"
      >
        <ellipse cx="12" cy="5" rx="9" ry="3" />
        <path d="M21 12c0 1.66-4.03 3-9 3s-9-1.34-9-3" />
        <path d="M3 5v14c0 1.66 4.03 3 9 3s9-1.34 9-3V5" />
      </svg>
    ),
    code: `eng, _ := engine.New(
  engine.WithStore(pgstore.New(bunDB)),
  engine.WithExtension(
    observability.NewMetricsExtension(),
  ),
  engine.WithLogger(slog.Default()),
)`,
    filename: "main.go",
  },
  {
    title: "Plugin Lifecycle Hooks",
    description:
      "OnRunCompleted, OnToolFailed, and 16 other lifecycle events. Wire in metrics, audit trails, or custom logic without modifying core code.",
    icon: (
      <svg
        className="size-5"
        viewBox="0 0 24 24"
        fill="none"
        stroke="currentColor"
        strokeWidth="1.5"
        strokeLinecap="round"
        strokeLinejoin="round"
        aria-hidden="true"
      >
        <path d="M20.24 12.24a6 6 0 00-8.49-8.49L5 10.5V19h8.5z" />
        <line x1="16" y1="8" x2="2" y2="22" />
        <line x1="17.5" y1="15" x2="9" y2="15" />
      </svg>
    ),
    code: `func (e *Audit) OnRunFailed(
  ctx context.Context,
  agentID id.AgentID,
  runID id.AgentRunID,
  err error,
) error {
  return e.record("run.failed", runID, err)
}`,
    filename: "extension.go",
  },
  {
    title: "Skills & Traits System",
    description:
      "Define skills with tool mastery levels and guidance notes. Define traits with dimensional values that influence agent tone, style, and behavior. Compose them into personas.",
    icon: (
      <svg
        className="size-5"
        viewBox="0 0 24 24"
        fill="none"
        stroke="currentColor"
        strokeWidth="1.5"
        strokeLinecap="round"
        strokeLinejoin="round"
        aria-hidden="true"
      >
        <path d="M3 6h18M3 12h18M3 18h18" />
        <rect x="2" y="3" width="20" height="18" rx="2" />
      </svg>
    ),
    code: `sk, _ := eng.CreateSkill(ctx, &skill.Skill{
  Name: "customer-support",
  Tools: []skill.ToolRef{
    {ToolName: "lookup_order",
     Mastery: "expert",
     Guidance: "Verify order ID format"},
  },
  DefaultProficiency: "proficient",
})
// sk.ID = skl_01h455...`,
    filename: "skill.go",
    colSpan: 2,
  },
];

const containerVariants = {
  hidden: {},
  visible: {
    transition: {
      staggerChildren: 0.08,
    },
  },
};

const itemVariants = {
  hidden: { opacity: 0, y: 20 },
  visible: {
    opacity: 1,
    y: 0,
    transition: { duration: 0.5, ease: "easeOut" as const },
  },
};

export function FeatureBento() {
  return (
    <section className="relative w-full py-20 sm:py-28">
      <div className="container max-w-(--fd-layout-width) mx-auto px-4 sm:px-6">
        <SectionHeader
          badge="Features"
          title="Everything you need for agent orchestration"
          description="Cortex handles the hard parts — personas, skills, traits, reasoning loops, and multi-tenancy — so you can focus on your application."
        />

        <motion.div
          variants={containerVariants}
          initial="hidden"
          whileInView="visible"
          viewport={{ once: true, margin: "-50px" }}
          className="mt-14 grid grid-cols-1 md:grid-cols-2 gap-4"
        >
          {features.map((feature) => (
            <motion.div
              key={feature.title}
              variants={itemVariants}
              className={cn(
                "group relative rounded-xl border border-fd-border bg-fd-card/50 backdrop-blur-sm p-6 hover:border-violet-500/20 hover:bg-fd-card/80 transition-all duration-300",
                feature.colSpan === 2 && "md:col-span-2",
              )}
            >
              {/* Header */}
              <div className="flex items-start gap-3 mb-4">
                <div className="flex items-center justify-center size-9 rounded-lg bg-violet-500/10 text-violet-600 dark:text-violet-400 shrink-0">
                  {feature.icon}
                </div>
                <div>
                  <h3 className="text-sm font-semibold text-fd-foreground">
                    {feature.title}
                  </h3>
                  <p className="text-xs text-fd-muted-foreground mt-1 leading-relaxed">
                    {feature.description}
                  </p>
                </div>
              </div>

              {/* Code snippet */}
              <CodeBlock
                code={feature.code}
                filename={feature.filename}
                showLineNumbers={false}
                className="text-xs"
              />
            </motion.div>
          ))}
        </motion.div>
      </div>
    </section>
  );
}
