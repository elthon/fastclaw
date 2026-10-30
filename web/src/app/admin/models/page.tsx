"use client";

import ModelsPage from "@/app/console/models/page";

// Deployment-wide model defaults; /console/models is the account's own.
export default function AdminModelsPage() {
  return <ModelsPage scope="system" />;
}
