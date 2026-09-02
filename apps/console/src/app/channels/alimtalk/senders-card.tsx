"use client";

import { useMutation } from "@tanstack/react-query";
import { useState } from "react";
import type { AlimtalkSender } from "@onda/api-client";
import { api } from "@/lib/api";
import { Button } from "@/components/ui/button";
import { Card, CardContent, CardHeader, CardTitle } from "@/components/ui/card";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { serverMessage } from "./alimtalk-labels";

/** 카카오 발신프로필 키는 40자 고정이다. 벤더에 보내기 전에 여기서 걸러 준다. */
const SENDER_KEY_LENGTH = 40;

/**
 * 발신프로필(카카오 채널) 관리 — 앱당 여러 개, 하나가 기본.
 * 저니 알림톡 노드의 발신프로필 select가 이 목록을 쓴다.
 */
export function SendersCard({
  appId,
  senders,
  pending,
  onChanged,
}: {
  appId: string | undefined;
  senders: AlimtalkSender[];
  pending: boolean;
  onChanged: () => void;
}) {
  const [senderKey, setSenderKey] = useState("");
  const [channelName, setChannelName] = useState("");
  const [isDefault, setIsDefault] = useState(false);
  const [msg, setMsg] = useState<string | null>(null);

  const create = useMutation({
    mutationFn: () => {
      if (!appId) throw new Error("앱을 찾을 수 없습니다");
      return api.alimtalk.senders.create(appId, {
        sender_key: senderKey.trim(),
        channel_name: channelName.trim() || undefined,
        is_default: isDefault,
      });
    },
    onSuccess: () => {
      setSenderKey("");
      setChannelName("");
      setIsDefault(false);
      setMsg("발신프로필을 추가했습니다.");
      onChanged();
    },
    onError: (e) => setMsg(serverMessage(e, "추가에 실패했습니다")),
  });

  const remove = useMutation({
    mutationFn: (id: string) => {
      if (!appId) throw new Error("앱을 찾을 수 없습니다");
      return api.alimtalk.senders.remove(appId, id);
    },
    onSuccess: () => {
      setMsg("발신프로필을 삭제했습니다. 이 프로필을 쓰던 템플릿 캐시도 함께 사라집니다.");
      onChanged();
    },
    onError: (e) => setMsg(serverMessage(e, "삭제에 실패했습니다")),
  });

  const keyLength = senderKey.trim().length;
  const keyValid = keyLength === SENDER_KEY_LENGTH;

  return (
    <Card>
      <CardHeader className="p-4">
        <CardTitle className="text-sm">발신프로필</CardTitle>
      </CardHeader>
      <CardContent className="flex flex-col gap-3 p-4 pt-0 text-sm">
        {pending ? (
          <p className="text-xs text-muted-foreground">불러오는 중…</p>
        ) : senders.length === 0 ? (
          <p className="text-xs text-muted-foreground">
            등록된 발신프로필이 없습니다. 카카오 비즈메시지에서 발급받은 발신프로필 키를 추가하세요.
          </p>
        ) : (
          <ul className="flex flex-col divide-y divide-border rounded-md border border-border">
            {senders.map((sender) => (
              <li key={sender.id} className="flex items-center justify-between gap-2 p-2">
                <div className="min-w-0">
                  <p className="truncate text-sm font-medium">
                    {sender.channel_name || "(채널명 없음)"}
                    {sender.is_default && <span className="ml-2 text-xs text-primary">기본</span>}
                    {sender.status === "disabled" && (
                      <span className="ml-2 text-xs text-muted-foreground">사용 중지</span>
                    )}
                  </p>
                  <code className="block truncate text-xs text-muted-foreground" title={sender.sender_key}>
                    {sender.sender_key}
                  </code>
                </div>
                <Button
                  variant="outline"
                  className="h-7 shrink-0 px-2 text-xs"
                  disabled={remove.isPending}
                  onClick={() => remove.mutate(sender.id)}
                >
                  삭제
                </Button>
              </li>
            ))}
          </ul>
        )}

        <div className="flex flex-col gap-2 rounded-md border border-border p-2">
          <p className="text-xs font-medium">발신프로필 추가</p>
          <div className="flex flex-col gap-1">
            <Label htmlFor="sender-key" className="text-xs">
              발신프로필 키 (40자)
            </Label>
            <Input
              id="sender-key"
              value={senderKey}
              maxLength={SENDER_KEY_LENGTH}
              placeholder="카카오에서 발급받은 senderKey"
              onChange={(e) => setSenderKey(e.target.value)}
            />
            <p className={`text-xs ${keyLength > 0 && !keyValid ? "text-destructive" : "text-muted-foreground"}`}>
              {keyLength}/{SENDER_KEY_LENGTH}자
              {keyLength > 0 && !keyValid && " — 발신프로필 키는 40자여야 합니다."}
            </p>
          </div>
          <div className="flex flex-col gap-1">
            <Label htmlFor="sender-name" className="text-xs">
              채널명 (선택)
            </Label>
            <Input
              id="sender-name"
              value={channelName}
              placeholder="@온다"
              onChange={(e) => setChannelName(e.target.value)}
            />
          </div>
          <label className="flex items-center gap-2 text-xs">
            <input type="checkbox" checked={isDefault} onChange={(e) => setIsDefault(e.target.checked)} />
            기본 발신프로필로 지정
          </label>
          <Button
            className="mt-1"
            disabled={!appId || !keyValid || create.isPending}
            onClick={() => create.mutate()}
          >
            {create.isPending ? "추가 중…" : "발신프로필 추가"}
          </Button>
        </div>
        {msg && <p className="text-xs text-muted-foreground">{msg}</p>}
      </CardContent>
    </Card>
  );
}
