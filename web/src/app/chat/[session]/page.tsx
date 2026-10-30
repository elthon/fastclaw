// /chat/<session> — a conversation, private or group. AppShell resolves the
// session to its agent or group and renders the conversation; this page
// only gives Next a dynamic route to match.
//
// generateStaticParams: under output:'export' Next emits one .html per
// param tuple. We ship a single placeholder ("_") and the Go SPA handler
// serves it for any concrete session id at runtime.
export async function generateStaticParams() {
  return [{ session: "_" }];
}

export default function ChatSessionPage() {
  return null;
}
