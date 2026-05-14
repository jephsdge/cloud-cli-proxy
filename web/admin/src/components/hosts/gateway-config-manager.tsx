import { useEffect, useMemo, useState } from "react";
import { CheckCircle2, Loader2, Power, Save, Wand2, X } from "lucide-react";
import { toast } from "sonner";
import {
  useHostGatewayConfig,
  useUpdateHostGatewayConfig,
  type GatewayConfigMode,
} from "@/hooks/use-hosts";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Textarea } from "@/components/ui/textarea";

interface GatewayConfigManagerProps {
  hostId: string;
  hostStatus: string;
}

const sourceLabel: Record<string, string> = {
  egress_binding: "继承出口绑定",
  host_outbound_override: "主机出口覆盖",
  host_full_config: "完整配置覆盖",
  host_custom_config: "自定义完整配置",
};

function formatConfig(value: Record<string, unknown> | null | undefined) {
  return value ? JSON.stringify(value, null, 2) : "";
}

function parseConfig(text: string): Record<string, unknown> | null {
  const trimmed = text.trim();
  if (!trimmed) return null;

  const parsed = JSON.parse(trimmed);
  if (!parsed || Array.isArray(parsed) || typeof parsed !== "object") {
    throw new Error("Gateway 配置必须是 JSON 对象");
  }
  return parsed as Record<string, unknown>;
}

function errorMessage(err: unknown) {
  if (err instanceof Error) return err.message;
  return "保存失败";
}

export function GatewayConfigManager({
  hostId,
  hostStatus,
}: GatewayConfigManagerProps) {
  const isRunning = hostStatus === "running";
  const { data, isLoading } = useHostGatewayConfig(hostId);
  const updateMutation = useUpdateHostGatewayConfig(hostId);
  const [mode, setMode] = useState<GatewayConfigMode>("auto");
  const [text, setText] = useState("");
  const [parseError, setParseError] = useState("");

  const savedMode = data?.mode ?? "auto";
  const savedText = useMemo(
    () => formatConfig(data?.gateway_config),
    [data?.gateway_config],
  );
  const effectiveText = useMemo(
    () => formatConfig(data?.effective_config),
    [data?.effective_config],
  );
  const hasChanges =
    mode !== savedMode ||
    (mode === "custom" && text.trim() !== savedText.trim());

  useEffect(() => {
    if (!data) return;
    setMode(data.mode ?? "auto");
    setText(formatConfig(data.gateway_config));
    setParseError("");
  }, [data]);

  function handleSave() {
    if (mode === "auto") {
      setParseError("");
      updateMutation.mutate(
        { mode: "auto", gateway_config: null },
        {
          onSuccess: (result) => {
            setMode(result.mode ?? "auto");
            setText(formatConfig(result.gateway_config));
            toast.success(
              result.applied ? "Gateway 配置已保存并应用" : "Gateway 配置已保存",
            );
          },
          onError: (err) => toast.error(errorMessage(err)),
        },
      );
      return;
    }

    let parsed: Record<string, unknown> | null;
    try {
      parsed = parseConfig(text);
    } catch (err) {
      setParseError(errorMessage(err));
      return;
    }
    if (!parsed) {
      setParseError("自定义 Gateway 配置不能为空");
      return;
    }

    setParseError("");
    updateMutation.mutate({ mode: "custom", gateway_config: parsed }, {
      onSuccess: (result) => {
        setMode(result.mode ?? "custom");
        setText(formatConfig(result.gateway_config));
        toast.success(
          result.applied ? "Gateway 配置已保存并应用" : "Gateway 配置已保存",
        );
      },
      onError: (err) => toast.error(errorMessage(err)),
    });
  }

  function handleFormat() {
    try {
      setText(formatConfig(parseConfig(text)));
      setParseError("");
    } catch (err) {
      setParseError(errorMessage(err));
    }
  }

  function handleClear() {
    setText("");
    setParseError("");
  }

  function handleToggleCustom() {
    setParseError("");
    if (mode === "custom") {
      setMode("auto");
      return;
    }
    setMode("custom");
    if (!text.trim()) {
      setText(savedText || effectiveText);
    }
  }

  if (isLoading) {
    return (
      <div className="flex items-center gap-2 py-3 text-sm text-muted-foreground">
        <Loader2 className="h-4 w-4 animate-spin" />
        加载 gateway 配置...
      </div>
    );
  }

  return (
    <div className="space-y-3">
      <div className="flex flex-wrap items-center gap-2">
        <Badge variant="outline">
          {sourceLabel[data?.source ?? "egress_binding"] ?? data?.source}
        </Badge>
        {isRunning ? (
          <Badge variant="secondary">运行中保存会重启 gateway</Badge>
        ) : null}
        {data?.applied ? (
          <span className="inline-flex items-center gap-1 text-xs text-emerald-700">
            <CheckCircle2 className="h-3.5 w-3.5" />
            已应用
          </span>
        ) : null}
        <Button
          type="button"
          size="sm"
          variant={mode === "custom" ? "secondary" : "outline"}
          className="h-7 gap-1.5"
          onClick={handleToggleCustom}
          disabled={updateMutation.isPending}
        >
          <Power className="h-3.5 w-3.5" />
          {mode === "custom" ? "关闭自定义配置" : "启用自定义配置"}
        </Button>
      </div>

      {mode === "custom" ? (
        <>
          <Textarea
            value={text}
            onChange={(event) => setText(event.target.value)}
            spellCheck={false}
            placeholder='{"inbounds":[...],"outbounds":[...],"route":{"final":"..."}}'
            className="min-h-56 resize-y font-mono text-xs leading-5"
          />

          {parseError ? (
            <p className="text-xs text-destructive">{parseError}</p>
          ) : (
            <p className="text-xs text-muted-foreground">
              自定义模式会按完整 sing-box JSON 保存，必须包含 tun inbound、outbounds 和 route.final。
            </p>
          )}

          <div className="flex flex-wrap items-center gap-2">
            <Button
              type="button"
              size="sm"
              className="gap-1.5"
              onClick={handleSave}
              disabled={!hasChanges || !text.trim() || updateMutation.isPending}
            >
              {updateMutation.isPending ? (
                <Loader2 className="h-3.5 w-3.5 animate-spin" />
              ) : (
                <Save className="h-3.5 w-3.5" />
              )}
              保存配置
            </Button>
            <Button
              type="button"
              size="sm"
              variant="outline"
              className="gap-1.5"
              onClick={handleFormat}
              disabled={!text.trim() || updateMutation.isPending}
            >
              <Wand2 className="h-3.5 w-3.5" />
              格式化
            </Button>
            <Button
              type="button"
              size="sm"
              variant="ghost"
              className="gap-1.5"
              onClick={handleClear}
              disabled={!text.trim() || updateMutation.isPending}
            >
              <X className="h-3.5 w-3.5" />
              清空
            </Button>
          </div>
        </>
      ) : (
        <div className="flex flex-wrap items-center gap-2 rounded-md border border-border/60 bg-muted/20 px-3 py-2">
          <span className="text-xs text-muted-foreground">
            当前使用出口绑定自动生成的 Gateway 配置。
          </span>
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
            保存自动模式
          </Button>
        </div>
      )}

      {effectiveText ? (
        <details className="rounded-lg border border-border/60 bg-muted/20 px-3 py-2">
          <summary className="cursor-pointer text-xs font-medium text-muted-foreground">
            查看当前生效配置
          </summary>
          <pre className="mt-3 max-h-80 overflow-auto whitespace-pre-wrap break-words rounded-md bg-background/80 p-3 font-mono text-xs leading-5">
            {effectiveText}
          </pre>
        </details>
      ) : null}
    </div>
  );
}
