import { useEffect, useMemo, useState } from "react";
import type { ReactNode } from "react";
import { Copy, Loader2, RefreshCw, Save, Wand2 } from "lucide-react";
import { toast } from "sonner";
import { TIMEZONE_OPTIONS } from "@/lib/timezones";
import {
  useHostIdentity,
  useUpdateHostIdentity,
  type WorkerIdentity,
} from "@/hooks/use-hosts";
import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import { Badge } from "@/components/ui/badge";
import {
  Select,
  SelectContent,
  SelectItem,
  SelectTrigger,
  SelectValue,
} from "@/components/ui/select";

interface IdentityManagerProps {
  hostId: string;
  hostStatus: string;
  fallbackIdentity: WorkerIdentity;
}

const identityDefaults: Required<WorkerIdentity> = {
  hostname: "",
  timezone: "America/New_York",
  machine_id: "",
  locale: {
    LANG: "en_US.UTF-8",
    LANGUAGE: "en_US:en",
    LC_ALL: "en_US.UTF-8",
  },
  vnc_resolution: "1920x1080",
  browser_language: "en-US",
  browser_window_size: "1920x1080",
};

function mergeIdentity(identity?: WorkerIdentity): Required<WorkerIdentity> {
  return {
    ...identityDefaults,
    ...identity,
    locale: {
      ...identityDefaults.locale,
      ...(identity?.locale ?? {}),
    },
  };
}

function makeMachineId() {
  const bytes = new Uint8Array(16);
  crypto.getRandomValues(bytes);
  return Array.from(bytes, (byte) => byte.toString(16).padStart(2, "0")).join("");
}

function identityJSON(identity: Required<WorkerIdentity>) {
  return JSON.stringify(identity, null, 2);
}

function isSameIdentity(a: Required<WorkerIdentity>, b: Required<WorkerIdentity>) {
  return identityJSON(a) === identityJSON(b);
}

function errorMessage(err: unknown) {
  if (err instanceof Error) return err.message;
  return "保存失败";
}

function isRecord(value: unknown): value is Record<string, unknown> {
  return typeof value === "object" && value !== null;
}

async function copyText(text: string) {
  if (navigator.clipboard?.writeText) {
    try {
      await navigator.clipboard.writeText(text);
      return;
    } catch {
      // Fall through to the legacy path. HTTP deployments often block
      // navigator.clipboard even when execCommand still works.
    }
  }

  const textarea = document.createElement("textarea");
  textarea.value = text;
  textarea.setAttribute("readonly", "true");
  textarea.style.position = "fixed";
  textarea.style.left = "-9999px";
  textarea.style.top = "0";
  textarea.style.opacity = "0";
  document.body.appendChild(textarea);
  textarea.focus();
  textarea.select();
  textarea.setSelectionRange(0, text.length);

  try {
    const ok = document.execCommand("copy");
    if (!ok) {
      throw new Error("浏览器拒绝复制，请手动复制");
    }
  } finally {
    document.body.removeChild(textarea);
  }
}

async function readTextFromClipboard() {
  if (navigator.clipboard?.readText) {
    try {
      return await navigator.clipboard.readText();
    } catch {
      // Fall through to manual paste. Clipboard reads are commonly blocked on
      // HTTP pages or when the browser does not grant clipboard permission.
    }
  }

  const text = window.prompt("浏览器无法直接读取剪贴板，请在这里粘贴系统指纹 JSON");
  if (text === null) {
    throw new Error("已取消粘贴");
  }
  return text;
}

export function IdentityManager({
  hostId,
  hostStatus,
  fallbackIdentity,
}: IdentityManagerProps) {
  const { data, isLoading, refetch } = useHostIdentity(hostId);
  const updateMutation = useUpdateHostIdentity(hostId);

  const dataIdentityKey = JSON.stringify(data?.identity ?? null);
  const fallbackIdentityKey = JSON.stringify(fallbackIdentity);
  const savedIdentity = useMemo(
    () => mergeIdentity(data?.identity ?? fallbackIdentity),
    [dataIdentityKey, fallbackIdentityKey],
  );
  const [draft, setDraft] = useState<Required<WorkerIdentity>>(savedIdentity);
  const requiresRebuild =
    data?.requires_rebuild ?? ["running", "stopped", "failed"].includes(hostStatus);
  const hasChanges = !isSameIdentity(draft, savedIdentity);

  useEffect(() => {
    setDraft(savedIdentity);
  }, [savedIdentity]);

  function updateField<K extends keyof Required<WorkerIdentity>>(
    field: K,
    value: Required<WorkerIdentity>[K],
  ) {
    setDraft((current) => ({ ...current, [field]: value }));
  }

  function updateLocale(field: keyof Required<WorkerIdentity>["locale"], value: string) {
    setDraft((current) => ({
      ...current,
      locale: {
        ...current.locale,
        [field]: value,
      },
    }));
  }

  function handleSave() {
    updateMutation.mutate(draft, {
      onSuccess: (result) => {
        toast.success(
          result.requires_rebuild
            ? "系统指纹已保存，重建主机后生效"
            : "系统指纹已保存",
        );
      },
      onError: (err) => toast.error(errorMessage(err)),
    });
  }

  async function handleCopy() {
    try {
      await copyText(identityJSON(draft));
      toast.success("系统指纹 JSON 已复制");
    } catch (err) {
      toast.error(errorMessage(err));
    }
  }

  async function handlePaste() {
    try {
      const text = await readTextFromClipboard();
      if (!text.trim()) {
        throw new Error("粘贴内容为空");
      }
      const parsed = JSON.parse(text) as unknown;
      const wrappedIdentity =
        isRecord(parsed) && isRecord(parsed.identity)
          ? (parsed.identity as WorkerIdentity)
          : undefined;
      const identity = wrappedIdentity ?? (isRecord(parsed) ? (parsed as WorkerIdentity) : {});
      setDraft(mergeIdentity(identity));
      toast.success("已从剪贴板载入");
    } catch (err) {
      toast.error(errorMessage(err));
    }
  }

  if (isLoading) {
    return (
      <div className="flex items-center gap-2 text-sm text-muted-foreground">
        <Loader2 className="h-4 w-4 animate-spin" />
        加载系统指纹...
      </div>
    );
  }

  return (
    <div className="space-y-4">
      <div className="grid gap-3 md:grid-cols-2 xl:grid-cols-3">
        <Field label="Hostname">
          <Input
            value={draft.hostname}
            onChange={(event) => updateField("hostname", event.target.value)}
            placeholder="cloud-worker"
          />
        </Field>
        <Field label="Timezone">
          <Select
            value={draft.timezone}
            onValueChange={(value) => updateField("timezone", value)}
          >
            <SelectTrigger>
              <SelectValue placeholder="选择时区" />
            </SelectTrigger>
            <SelectContent>
              {TIMEZONE_OPTIONS.map((tz) => (
                <SelectItem key={tz.value} value={tz.value}>
                  {tz.label}
                  <span className="ml-1.5 text-muted-foreground">({tz.offset})</span>
                </SelectItem>
              ))}
            </SelectContent>
          </Select>
        </Field>
        <Field label="Machine ID">
          <div className="flex gap-2">
            <Input
              value={draft.machine_id}
              onChange={(event) => updateField("machine_id", event.target.value)}
              className="font-mono"
              placeholder="32 hex chars"
            />
            <Button
              type="button"
              variant="outline"
              size="icon"
              onClick={() => updateField("machine_id", makeMachineId())}
              title="生成 machine-id"
            >
              <Wand2 className="h-4 w-4" />
            </Button>
          </div>
        </Field>
        <Field label="LANG">
          <Input
            value={draft.locale.LANG}
            onChange={(event) => updateLocale("LANG", event.target.value)}
            className="font-mono"
          />
        </Field>
        <Field label="LANGUAGE">
          <Input
            value={draft.locale.LANGUAGE}
            onChange={(event) => updateLocale("LANGUAGE", event.target.value)}
            className="font-mono"
          />
        </Field>
        <Field label="LC_ALL">
          <Input
            value={draft.locale.LC_ALL}
            onChange={(event) => updateLocale("LC_ALL", event.target.value)}
            className="font-mono"
          />
        </Field>
        <Field label="VNC Resolution">
          <Input
            value={draft.vnc_resolution}
            onChange={(event) => updateField("vnc_resolution", event.target.value)}
            className="font-mono"
          />
        </Field>
        <Field label="Browser Language">
          <Input
            value={draft.browser_language}
            onChange={(event) => updateField("browser_language", event.target.value)}
            className="font-mono"
          />
        </Field>
        <Field label="Browser Window">
          <Input
            value={draft.browser_window_size}
            onChange={(event) => updateField("browser_window_size", event.target.value)}
            className="font-mono"
          />
        </Field>
      </div>

      <div className="flex flex-wrap items-center gap-2">
        <Button
          type="button"
          size="sm"
          className="gap-1.5"
          onClick={handleSave}
          disabled={!hasChanges || updateMutation.isPending}
        >
          {updateMutation.isPending ? (
            <Loader2 className="h-3.5 w-3.5 animate-spin" />
          ) : (
            <Save className="h-3.5 w-3.5" />
          )}
          保存指纹
        </Button>
        <Button
          type="button"
          variant="outline"
          size="sm"
          className="gap-1.5"
          onClick={handleCopy}
        >
          <Copy className="h-3.5 w-3.5" />
          复制 JSON
        </Button>
        <Button
          type="button"
          variant="outline"
          size="sm"
          onClick={handlePaste}
        >
          粘贴 JSON
        </Button>
        <Button
          type="button"
          variant="ghost"
          size="sm"
          className="gap-1.5"
          onClick={() => refetch()}
        >
          <RefreshCw className="h-3.5 w-3.5" />
          刷新
        </Button>
        {requiresRebuild && (
          <Badge variant="secondary">重建后进入容器生效</Badge>
        )}
      </div>
    </div>
  );
}

function Field({
  label,
  children,
}: {
  label: string;
  children: ReactNode;
}) {
  return (
    <label className="space-y-1.5 text-xs font-medium text-muted-foreground">
      <span>{label}</span>
      {children}
    </label>
  );
}
