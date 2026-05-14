import { useEffect, useState } from "react";
import { Loader2, Save } from "lucide-react";
import { toast } from "sonner";
import { TIMEZONE_OPTIONS } from "@/lib/timezones";
import { useUpdateHostTimezone } from "@/hooks/use-hosts";
import { Button } from "@/components/ui/button";
import {
  Select,
  SelectContent,
  SelectItem,
  SelectTrigger,
  SelectValue,
} from "@/components/ui/select";

interface TimezoneManagerProps {
  hostId: string;
  hostStatus: string;
  timezone: string;
}

function errorMessage(err: unknown) {
  if (err instanceof Error) return err.message;
  return "保存失败";
}

export function TimezoneManager({
  hostId,
  hostStatus,
  timezone,
}: TimezoneManagerProps) {
  const [value, setValue] = useState(timezone || "America/New_York");
  const updateTimezoneMutation = useUpdateHostTimezone(hostId);
  const existingContainerNeedsRebuild = ["running", "stopped", "failed"].includes(hostStatus);
  const hasChanges = value !== timezone;

  useEffect(() => {
    setValue(timezone || "America/New_York");
  }, [timezone]);

  function handleSave() {
    updateTimezoneMutation.mutate(value, {
      onSuccess: (result) => {
        toast.success(
          result.requires_rebuild
            ? "时区已保存，重建后进入容器生效"
            : "时区已保存",
        );
      },
      onError: (err) => toast.error(errorMessage(err)),
    });
  }

  return (
    <div className="space-y-3">
      <Select
        value={value}
        onValueChange={setValue}
        disabled={updateTimezoneMutation.isPending}
      >
        <SelectTrigger className="h-9">
          <SelectValue placeholder="选择时区" />
        </SelectTrigger>
        <SelectContent>
          {TIMEZONE_OPTIONS.map((tz) => (
            <SelectItem key={tz.value} value={tz.value}>
              {tz.label}
              <span className="ml-1.5 text-muted-foreground">
                ({tz.offset})
              </span>
            </SelectItem>
          ))}
        </SelectContent>
      </Select>

      <div className="flex flex-wrap items-center gap-2">
        <Button
          type="button"
          size="sm"
          className="gap-1.5"
          onClick={handleSave}
          disabled={!hasChanges || updateTimezoneMutation.isPending}
        >
          {updateTimezoneMutation.isPending ? (
            <Loader2 className="h-3.5 w-3.5 animate-spin" />
          ) : (
            <Save className="h-3.5 w-3.5" />
          )}
          保存时区
        </Button>
      </div>

      <p className="text-xs text-muted-foreground">
        {existingContainerNeedsRebuild
          ? "已有容器需重建后进入新时区"
          : "创建容器时会使用该时区"}
      </p>
    </div>
  );
}
