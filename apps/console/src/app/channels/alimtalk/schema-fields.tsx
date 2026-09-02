"use client";

import type { SchemaField } from "./connector-schema";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";

/**
 * 매니페스트 JSON Schema로 그린 입력 폼.
 * 필드 목록은 connector-schema.ts가 순수 함수로 만들고, 여기서는 위젯만 고른다 —
 * 벤더가 늘어도 이 파일은 그대로다.
 */
export function SchemaFields({
  fields,
  values,
  disabled,
  idPrefix,
  hint,
  onChange,
}: {
  fields: SchemaField[];
  values: Record<string, string>;
  disabled?: boolean;
  idPrefix: string;
  /** 값이 어디에 저장되는지 한 줄로 — 비밀을 조용히 버리지 않는다는 원칙을 화면에 드러낸다. */
  hint?: (field: SchemaField) => string;
  onChange: (name: string, value: string) => void;
}) {
  return (
    <>
      {fields.map((field) => {
        const id = `${idPrefix}-${field.name}`;
        return (
          <div key={field.name} className="flex flex-col gap-1">
            <Label htmlFor={id} className="text-xs">
              {field.label}
              {field.required && <span className="ml-1 text-destructive">*</span>}
              {field.secret && <span className="ml-1 text-muted-foreground">(비밀)</span>}
            </Label>
            {field.kind === "select" ? (
              <select
                id={id}
                className="h-9 rounded-md border border-border bg-card px-2 text-sm"
                value={values[field.name] ?? ""}
                disabled={disabled}
                onChange={(e) => onChange(field.name, e.target.value)}
              >
                {(field.options ?? []).map((option) => (
                  <option key={option} value={option}>
                    {option}
                  </option>
                ))}
              </select>
            ) : (
              <Input
                id={id}
                type={field.kind === "password" ? "password" : "text"}
                value={values[field.name] ?? ""}
                disabled={disabled}
                placeholder={field.placeholder}
                autoComplete={field.kind === "password" ? "new-password" : "off"}
                onChange={(e) => onChange(field.name, e.target.value)}
              />
            )}
            {field.description && <p className="text-xs text-muted-foreground">{field.description}</p>}
            {hint && <p className="text-[11px] text-muted-foreground">{hint(field)}</p>}
          </div>
        );
      })}
    </>
  );
}
