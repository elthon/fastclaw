"use client";

import { cn } from "@/lib/utils";
import { MobileAreaMenu } from "@/components/app-rail";

// SidebarTitle names the area a sidebar belongs to (Chat / Console /
// Admin). The FastClaw logo and the account live in the AppRail beside it;
// on phones, where the rail is hidden, the title opens the area menu.
// Sized like the right-hand panel titles (Workspace) so both edges match.
export function SidebarTitle({
  title,
  className,
  children,
}: {
  title: string;
  className?: string;
  children?: React.ReactNode;
}) {
  return (
    <div className={cn("flex h-14 items-center gap-2 px-2", className)}>
      <h2 className="hidden truncate text-[15px] font-semibold text-foreground md:block group-data-[collapsible=icon]:hidden">
        {title}
      </h2>
      {/* Phones have no AppRail, so the title switches areas there. */}
      <div className="min-w-0 md:hidden">
        <MobileAreaMenu title={title} />
      </div>
      {children}
    </div>
  );
}
