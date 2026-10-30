"use client";

import * as React from "react";
import { ChevronsLeft, ChevronsRight, X } from "lucide-react";
import { useLocale } from "@/components/locale-provider";
import { Tooltip, TooltipContent, TooltipTrigger } from "@/components/ui/tooltip";

// Same geometry and saved width as the private chat's Agent panel, so the
// right panel keeps its size when switching between a DM and a group.
const PANEL_MIN_WIDTH = 300;
const PANEL_DEFAULT_WIDTH = 400;
const PANEL_MAX_WIDTH = 520;
const CHAT_PANE_MIN_WIDTH = 520;
const PANEL_WIDTH_KEY = "fastclaw:bot-panel-width";

export const RIGHT_PANEL_ICON_BUTTON =
  "flex size-8 shrink-0 items-center justify-center rounded-lg text-muted-foreground transition hover:bg-black/5 hover:text-foreground focus-visible:ring-2 focus-visible:ring-ring dark:hover:bg-white/8";

export const RIGHT_PANEL_CARD_BG = (active: boolean) =>
  active
    ? "bg-black/[0.065] dark:bg-white/[0.11]"
    : "hover:bg-black/[0.04] dark:hover:bg-white/[0.07]";

// RightPanelToggle is the page-header button that opens and closes the
// right panel.
// RightPanelToggle is the header button for the right panel, shared by the
// private and group chats. Double chevrons show the action — « opens the
// panel, » closes it — so it doesn't read as the left sidebar's mirror.
export function RightPanelToggle({ open, label, onToggle }: {
  open: boolean;
  label: string;
  onToggle: () => void;
}) {
  return (
    <button
      type="button"
      onClick={onToggle}
      className={`inline-flex size-9 shrink-0 items-center justify-center rounded-xl transition-colors ${
        open ? "bg-muted text-foreground" : "text-muted-foreground hover:bg-muted/70 hover:text-foreground"
      }`}
      title={label}
      aria-label={label}
      aria-pressed={open}
    >
      {open ? <ChevronsRight className="size-[18px]" /> : <ChevronsLeft className="size-[18px]" />}
    </button>
  );
}

// RightPanel is the resizable side panel shell: a drawer with a backdrop
// below xl, an in-flow column with a drag handle from xl up.
export function RightPanel({ title, label, onClose, children }: {
  title: string;
  label: string;
  onClose: () => void;
  children: React.ReactNode;
}) {
  const { tr } = useLocale();
  const [panelWidth, setPanelWidth] = React.useState(() => {
    if (typeof window === "undefined") return PANEL_DEFAULT_WIDTH;
    const stored = Number(window.localStorage.getItem(PANEL_WIDTH_KEY));
    return Number.isFinite(stored) && stored >= PANEL_MIN_WIDTH && stored <= PANEL_MAX_WIDTH
      ? stored
      : PANEL_DEFAULT_WIDTH;
  });
  const [resizing, setResizing] = React.useState(false);
  const panelRef = React.useRef<HTMLElement>(null);
  const resizeStartRef = React.useRef({ x: 0, width: PANEL_DEFAULT_WIDTH });
  const latestPanelWidthRef = React.useRef(panelWidth);

  const clampPanelWidth = React.useCallback((candidate: number) => {
    const containerWidth = panelRef.current?.parentElement?.getBoundingClientRect().width
      || window.innerWidth;
    const responsiveMax = Math.max(PANEL_MIN_WIDTH, containerWidth - CHAT_PANE_MIN_WIDTH);
    return Math.round(Math.max(PANEL_MIN_WIDTH, Math.min(PANEL_MAX_WIDTH, responsiveMax, candidate)));
  }, []);

  const savePanelWidth = (width: number) => {
    try {
      window.localStorage.setItem(PANEL_WIDTH_KEY, String(width));
    } catch {
      // Ignore private-mode and quota failures.
    }
  };

  const commitPanelWidth = (width: number) => {
    const next = clampPanelWidth(width);
    latestPanelWidthRef.current = next;
    setPanelWidth(next);
    savePanelWidth(next);
  };

  React.useEffect(() => {
    if (!resizing) return;
    const handleMove = (event: PointerEvent) => {
      const next = clampPanelWidth(resizeStartRef.current.width + resizeStartRef.current.x - event.clientX);
      latestPanelWidthRef.current = next;
      setPanelWidth(next);
    };
    const handleUp = () => {
      setResizing(false);
      savePanelWidth(latestPanelWidthRef.current);
    };
    document.body.style.cursor = "col-resize";
    document.body.style.userSelect = "none";
    window.addEventListener("pointermove", handleMove);
    window.addEventListener("pointerup", handleUp, { once: true });
    return () => {
      window.removeEventListener("pointermove", handleMove);
      window.removeEventListener("pointerup", handleUp);
      document.body.style.cursor = "";
      document.body.style.userSelect = "";
    };
  }, [clampPanelWidth, resizing]);

  return (
    <>
      <button
        type="button"
        className="fixed inset-0 z-40 bg-black/20 xl:hidden"
        onClick={onClose}
        aria-label={tr("Close {{name}}", "关闭{{name}}", { name: label })}
      />
      <aside
        ref={panelRef}
        aria-label={label}
        data-resizable-right-panel="true"
        style={{ "--bot-panel-width": `${panelWidth}px` } as React.CSSProperties}
        className="fixed inset-y-0 right-0 z-50 flex h-dvh w-[min(94vw,420px)] shrink-0 flex-col border-l border-border bg-background shadow-2xl xl:relative xl:z-30 xl:-mt-14 xl:h-screen xl:w-[var(--bot-panel-width)] xl:min-w-[300px] xl:max-w-[calc(100%_-_520px)] xl:shadow-none"
      >
        <button
          type="button"
          className={`absolute inset-y-0 left-0 z-40 hidden w-3 -translate-x-1/2 cursor-col-resize touch-none outline-none after:absolute after:inset-y-0 after:left-1/2 after:w-px after:transition-colors hover:after:bg-foreground/25 focus-visible:after:bg-ring xl:block ${
            resizing ? "after:bg-ring" : "after:bg-transparent"
          }`}
          onPointerDown={(event) => {
            event.preventDefault();
            resizeStartRef.current = {
              x: event.clientX,
              width: panelRef.current?.getBoundingClientRect().width || panelWidth,
            };
            latestPanelWidthRef.current = resizeStartRef.current.width;
            setResizing(true);
          }}
          onDoubleClick={() => commitPanelWidth(PANEL_DEFAULT_WIDTH)}
          onKeyDown={(event) => {
            if (event.key === "ArrowLeft") {
              event.preventDefault();
              commitPanelWidth(panelWidth + 16);
            } else if (event.key === "ArrowRight") {
              event.preventDefault();
              commitPanelWidth(panelWidth - 16);
            } else if (event.key === "Home") {
              event.preventDefault();
              commitPanelWidth(PANEL_DEFAULT_WIDTH);
            }
          }}
          aria-label={tr("Resize right panel", "调整右侧栏宽度")}
          aria-orientation="vertical"
          aria-valuemin={PANEL_MIN_WIDTH}
          aria-valuemax={PANEL_MAX_WIDTH}
          aria-valuenow={panelWidth}
          role="separator"
          title={tr("Drag to resize · Double-click to reset", "拖动调整宽度 · 双击恢复默认宽度")}
        />
        <div className="flex h-14 shrink-0 items-center gap-0.5 pl-5 pr-3">
          <h2 className="min-w-0 flex-1 truncate text-[15px] font-semibold text-foreground">{title}</h2>
          <button
            type="button"
            onClick={onClose}
            className={RIGHT_PANEL_ICON_BUTTON}
            aria-label={tr("Close {{name}}", "关闭{{name}}", { name: label })}
            title={tr("Close", "关闭")}
          >
            <X className="size-[18px]" />
          </button>
        </div>
        {children}
      </aside>
    </>
  );
}

// RightPanelTabs is the segmented control under the panel title.
export function RightPanelTabs<T extends string>({ label, tabs, value, onChange }: {
  label: string;
  tabs: ReadonlyArray<readonly [T, string]>;
  value: T;
  onChange: (value: T) => void;
}) {
  return (
    <div className="shrink-0 px-4 pt-1">
      <div
        role="tablist"
        aria-label={label}
        className="grid rounded-[10px] bg-black/[0.045] p-[3px] dark:bg-white/[0.06]"
        style={{ gridTemplateColumns: `repeat(${tabs.length}, minmax(0, 1fr))` }}
      >
        {tabs.map(([id, text]) => {
          const selected = value === id;
          return (
            <button
              key={id}
              type="button"
              role="tab"
              aria-selected={selected}
              onClick={() => onChange(id)}
              className={`h-7 min-w-0 truncate rounded-[7px] px-2 text-[13px] transition-[background-color,color,box-shadow] focus-visible:ring-2 focus-visible:ring-ring ${
                selected
                  ? "bg-background font-medium text-foreground shadow-[0_1px_2px_rgba(0,0,0,0.06),0_1px_6px_-1px_rgba(0,0,0,0.08)] dark:bg-white/[0.12] dark:shadow-none"
                  : "text-muted-foreground hover:text-foreground"
              }`}
            >
              {text}
            </button>
          );
        })}
      </div>
    </div>
  );
}

// RightPanelListHeader sits above a panel list: the count on the left and
// the list's create action on the right.
export function RightPanelListHeader({ label, actionLabel, shortcut, icon: Icon, disabled, onAction }: {
  label: string;
  actionLabel: string;
  shortcut?: string;
  icon: React.ComponentType<{ className?: string }>;
  disabled?: boolean;
  onAction: () => void;
}) {
  return (
    <div className="mb-1 flex h-8 items-center justify-between pl-3 pr-1.5">
      <span className="text-[13px] text-muted-foreground">{label}</span>
      <Tooltip>
        <TooltipTrigger
          render={
            <button
              type="button"
              onClick={onAction}
              disabled={disabled}
              className="flex size-7 items-center justify-center rounded-lg text-muted-foreground transition hover:bg-black/5 hover:text-foreground focus-visible:ring-2 focus-visible:ring-ring disabled:opacity-40 dark:hover:bg-white/8"
              aria-label={actionLabel}
            >
              <Icon className="size-4" />
            </button>
          }
        />
        <TooltipContent side="left">
          <span>{actionLabel}</span>
          {shortcut && <kbd data-slot="kbd" className="bg-background/15 px-1.5 py-0.5 text-[10px]">{shortcut}</kbd>}
        </TooltipContent>
      </Tooltip>
    </div>
  );
}
