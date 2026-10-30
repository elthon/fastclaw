"use client";

import * as React from "react";
import { usePathname, useRouter } from "next/navigation";
import { ChevronDownIcon, LayoutDashboardIcon, LogOutIcon, MessagesSquareIcon, SettingsIcon, UserCogIcon } from "lucide-react";
import { Tooltip, TooltipContent, TooltipTrigger } from "@/components/ui/tooltip";
import {
  DropdownMenu,
  DropdownMenuContent,
  DropdownMenuGroup,
  DropdownMenuItem,
  DropdownMenuLabel,
  DropdownMenuSeparator,
  DropdownMenuTrigger,
} from "@/components/ui/dropdown-menu";
import { useLocale } from "@/components/locale-provider";
import { getMe, type MeResponse } from "@/lib/api";
import { logout } from "@/lib/auth";
import { areaHome, areaOf, resolveChatLanding, type Area } from "@/lib/landing";
import { cn } from "@/lib/utils";
import { useSidebarOptional } from "@/components/ui/sidebar";

// Width of the rail. SidebarLayout passes it to the sidebar as
// --sidebar-offset so the (fixed-position) sidebar sits right of it.
export const APP_RAIL_WIDTH = "3.5rem";

// AppRail is the far-left switcher between the web UI's areas — chat,
// console and (super_admin) admin: the FastClaw logo on top, the signed-in
// account (with Settings and Log out) at the bottom. Each area reopens where the
// user left it. Desktop only: on mobile the sidebar is a sheet.
export function AppRail() {
  const { tr } = useLocale();
  const { me, isAdmin, area, go } = useAreaNav();

  return (
    <div className="hidden w-(--app-rail-width) shrink-0 md:block" style={{ "--app-rail-width": APP_RAIL_WIDTH } as React.CSSProperties}>
      <nav
        aria-label={tr("Areas", "功能区")}
        className="fixed inset-y-0 left-0 z-20 flex w-(--app-rail-width) flex-col items-center gap-1 border-r border-sidebar-border bg-sidebar pb-3 pt-2"
      >
        <button
          type="button"
          onClick={() => go("chat")}
          aria-label="FastClaw"
          title="FastClaw"
          className="mb-3 flex size-10 items-center justify-center rounded-lg"
        >
          {/* eslint-disable-next-line @next/next/no-img-element */}
          <img src="/logo.png" alt="" width={32} height={32} draggable={false} className="size-8 select-none object-contain" />
        </button>
        <RailButton
          label={tr("Chat", "对话")}
          active={area === "chat"}
          onClick={() => go("chat")}
          icon={MessagesSquareIcon}
        />
        <RailButton
          label={tr("Console", "控制台")}
          active={area === "console"}
          onClick={() => go("console")}
          icon={LayoutDashboardIcon}
        />
        {isAdmin && (
          <RailButton
            label={tr("System", "系统")}
            active={area === "admin"}
            onClick={() => go("admin")}
            icon={UserCogIcon}
          />
        )}
        <div className="mt-auto">
          <RailAccount me={me} />
        </div>
      </nav>
    </div>
  );
}

function RailButton({
  label,
  active = false,
  onClick,
  icon: Icon,
}: {
  label: string;
  active?: boolean;
  onClick: () => void;
  icon: React.ComponentType<{ className?: string }>;
}) {
  return (
    <Tooltip>
      <TooltipTrigger
        render={
          <button
            type="button"
            aria-label={label}
            aria-current={active ? "page" : undefined}
            onClick={onClick}
            className={cn(
              "flex size-10 items-center justify-center rounded-lg text-muted-foreground transition-colors hover:bg-sidebar-accent hover:text-sidebar-accent-foreground",
              active && "bg-sidebar-accent text-sidebar-accent-foreground",
            )}
          />
        }
      >
        <Icon className="size-5" />
      </TooltipTrigger>
      <TooltipContent side="right">{label}</TooltipContent>
    </Tooltip>
  );
}

// RailAccount is the signed-in account's avatar with its menu.
function RailAccount({ me }: { me: MeResponse | null }) {
  const { t, tr } = useLocale();
  const name = me?.user?.displayName?.trim() || me?.user?.username || t("common.user");
  const role = me?.user?.role || tr("user", "用户");
  const initials = name.slice(0, 2).toUpperCase();
  const avatar = (size: string) =>
    me?.user?.avatarUrl ? (
      // eslint-disable-next-line @next/next/no-img-element
      <img src={me.user.avatarUrl} alt="" className={cn(size, "rounded-lg object-cover")} />
    ) : (
      <span className={cn(size, "flex items-center justify-center rounded-lg bg-emerald-500/20 text-xs font-bold text-emerald-500")}>
        {initials}
      </span>
    );
  return (
    <DropdownMenu>
      <DropdownMenuTrigger
        render={
          <button
            type="button"
            aria-label={name}
            title={name}
            className="flex size-10 items-center justify-center rounded-lg transition-colors hover:bg-sidebar-accent"
          />
        }
      >
        {avatar("size-8")}
      </DropdownMenuTrigger>
      <DropdownMenuContent side="right" align="end" sideOffset={8} className="min-w-56 rounded-lg">
        <DropdownMenuGroup>
          <DropdownMenuLabel className="p-0 font-normal">
            <div className="flex items-center gap-2 px-1 py-1.5 text-left text-sm">
              {avatar("size-8")}
              <div className="grid flex-1 leading-tight">
                <span className="truncate font-medium">{name}</span>
                <span className="truncate text-xs text-muted-foreground">{role}</span>
              </div>
            </div>
          </DropdownMenuLabel>
        </DropdownMenuGroup>
        <DropdownMenuSeparator />
        <DropdownMenuItem
          onClick={() => window.dispatchEvent(new CustomEvent("fastclaw:open-user-settings"))}
        >
          <SettingsIcon />
          <span>{t("common.settings")}</span>
        </DropdownMenuItem>
        <DropdownMenuSeparator />
        <DropdownMenuItem
          onClick={() => {
            logout();
            window.location.href = "/";
          }}
        >
          <LogOutIcon />
          <span>{t("common.logOut")}</span>
        </DropdownMenuItem>
      </DropdownMenuContent>
    </DropdownMenu>
  );
}

// useAreaNav is the area switching shared by the desktop rail and the
// mobile area menu: the signed-in account, the current area, and a jump
// that reopens an area where the user left it.
function useAreaNav() {
  const pathname = usePathname() || "";
  const router = useRouter();
  const [me, setMe] = React.useState<MeResponse | null>(null);
  const isAdmin = me?.user?.role === "super_admin";
  const area = areaOf(pathname);

  // Account settings live in a dialog, so a profile save doesn't remount
  // the rail; refresh the name/avatar on the same event the sidebar uses.
  React.useEffect(() => {
    const refresh = () => {
      getMe().then(setMe).catch(() => {});
    };
    refresh();
    window.addEventListener("fastclaw:user-profile-changed", refresh);
    return () => window.removeEventListener("fastclaw:user-profile-changed", refresh);
  }, []);

  const go = async (target: Area) => {
    if (target === area) return;
    router.push(target === "chat" ? await resolveChatLanding() : areaHome(target));
  };
  return { me, isAdmin, area, go };
}

// MobileAreaMenu stands in for the rail on phones, where the rail is
// hidden: the sidebar title opens a menu with the areas (the account sits
// in the sheet's footer).
export function MobileAreaMenu({ title }: { title: string }) {
  const { tr } = useLocale();
  const { isAdmin, area, go } = useAreaNav();
  // The mobile sidebar is a sheet; close it so the new area isn't hidden.
  const sidebar = useSidebarOptional();
  const switchTo = (id: Area) => {
    sidebar?.setOpenMobile(false);
    void go(id);
  };
  const areas: Array<{ id: Area; label: string; icon: React.ComponentType<{ className?: string }> }> = [
    { id: "chat", label: tr("Chat", "对话"), icon: MessagesSquareIcon },
    { id: "console", label: tr("Console", "控制台"), icon: LayoutDashboardIcon },
    ...(isAdmin ? [{ id: "admin" as Area, label: tr("System", "系统"), icon: UserCogIcon }] : []),
  ];
  return (
    <DropdownMenu>
      <DropdownMenuTrigger
        render={
          <button
            type="button"
            className="-ml-1 flex min-w-0 items-center gap-1 rounded-lg px-1 py-1 text-[15px] font-semibold text-foreground transition-colors hover:bg-sidebar-accent"
            aria-label={tr("Switch area", "切换功能区")}
          />
        }
      >
        <span className="truncate">{title}</span>
        <ChevronDownIcon className="size-4 shrink-0 text-muted-foreground" />
      </DropdownMenuTrigger>
      <DropdownMenuContent align="start" sideOffset={10} className="min-w-52 rounded-lg">
        {areas.map(({ id, label, icon: Icon }) => (
          <DropdownMenuItem key={id} onClick={() => switchTo(id)} aria-current={area === id ? "page" : undefined}
            className={cn(area === id && "bg-accent text-accent-foreground")}>
            <Icon />
            <span>{label}</span>
          </DropdownMenuItem>
        ))}
      </DropdownMenuContent>
    </DropdownMenu>
  );
}
