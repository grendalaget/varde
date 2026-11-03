import { api } from "../api/client";
import EventList from "../components/EventList";
import { Card, PageHeader } from "../components/ui";
import { useMergedEvents } from "../lib/activity";
import { useLoad } from "../lib/data";
import { useOnEvent } from "../lib/events";
import { useSession } from "../lib/session";

export default function Activity() {
  const { group } = useSession();
  const events = useMergedEvents((e) => e.type !== "execution.state");
  const { data, refresh } = useLoad(
    () =>
      api.GET("/v1/groups/{groupId}/servers", {
        params: { path: { groupId: group.group_id } },
      }),
    [group.group_id],
  );
  useOnEvent((e) => e.type === "server.created", refresh);
  const serverNames = Object.fromEntries(
    (data?.servers ?? []).map((s) => [s.id, s.name]),
  );
  return (
    <>
      <PageHeader
        title="Activity"
        subtitle="Everything that happened in this group, live."
      />
      <Card>
        <EventList events={events} showServer serverNames={serverNames} />
      </Card>
    </>
  );
}
