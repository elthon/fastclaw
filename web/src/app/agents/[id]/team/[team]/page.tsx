import { LegacyTeamChatRedirect } from "@/components/legacy-team-chat-redirect";

export async function generateStaticParams() {
  return [{ team: "_" }];
}

export default function TeamChatPage() {
  return <LegacyTeamChatRedirect />;
}
