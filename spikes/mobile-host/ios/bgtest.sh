#!/usr/bin/env bash
# usage: bgtest.sh <bundle-id> <label> <bg-seconds>
set -u
B=$1; L=$2; DUR=$3; OUT=~/spike-out/$L; mkdir -p $OUT
for b in games.varde.spike.mobilehost games.varde.spike.mobilehost.audio; do xcrun simctl terminate booted $b 2>/dev/null; done; sleep 2
curl -s -m1 -o /dev/null -w "pre-launch health: %{http_code}\n" http://127.0.0.1:18505/health
D=$(xcrun simctl get_app_container booted $B data); rm -f "$D/Documents/spike.log"
xcrun simctl launch booted $B ${4:-}; sleep 5
( while :; do
    t=$(gdate -u +%H:%M:%S.%3N 2>/dev/null || python3 -c 'import datetime;print(datetime.datetime.utcnow().strftime("%H:%M:%S.%f")[:-3])')
    c=$(curl -s -o /dev/null -m 0.4 -w '%{http_code}' http://127.0.0.1:18505/health); echo "$t $c"; sleep 0.5
  done ) > $OUT/curl.log 2>&1 &
CP=$!
sleep 3
echo "host: backgrounding at $(date -u +%H:%M:%S)" | tee $OUT/host.log
xcrun simctl launch booted com.apple.Preferences >/dev/null
sleep $DUR
echo "host: foregrounding at $(date -u +%H:%M:%S)" | tee -a $OUT/host.log
xcrun simctl launch booted $B >/dev/null
sleep 5
kill $CP
cp "$D/Documents/spike.log" $OUT/spike.log
PID=$(pgrep -f "/$( [ $L = audio ] && echo MobileHostSpikeAudio || echo MobileHostSpike).app/" | head -1); echo "pid now: $PID" >> $OUT/host.log
