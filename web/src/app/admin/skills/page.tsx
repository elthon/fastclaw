"use client";

import SkillsPage from "@/app/console/skills/page";

// Deployment-wide skills; /console/skills is the account's own.
export default function AdminSkillsPage() {
  return <SkillsPage scope="system" />;
}
