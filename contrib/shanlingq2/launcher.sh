#!/bin/sh
# Installed as /mnt/mmc/.airplay/run. Q2 Pod starts it from Now Playing's AirPlay button (a hidden card
# app, q2-pod's patch/addon.c). It runs from /tmp, never the card: the app, cliairplay and this launcher
# are copied there first and the card's copy of this script is left at once (exec), so nothing keeps
# the card busy and USB storage can always take it. The loop restarts the app after a failure, waiting
# 5 s and doubling to a minute; an exit of 0 (idle, or stopped) ends it. Q2 Pod stops it with a TERM to
# the loop's pid in /tmp/q2-airplay; the app lets the speaker go first.
pid=/tmp/q2-airplay
if [ "$1" != --loop ]; then
    [ -f $pid ] && kill -0 "$(cat $pid)" 2>/dev/null && exit 0
    dir=$(cd "$(dirname "$0")" && pwd)
    cp -f "$dir/q2-airplay" /tmp/q2-airplay.bin && chmod 755 /tmp/q2-airplay.bin || exit 1
    cp -f "$dir/cliairplay" /tmp/q2-airplay.cliairplay && chmod 755 /tmp/q2-airplay.cliairplay || exit 1
    cp -f "$0" /tmp/q2-airplay.run || exit 1
    cd /tmp
    exec /bin/sh /tmp/q2-airplay.run --loop "$dir"
fi
dir=$2
(
    child=
    trap '[ -n "$child" ] && kill -TERM $child 2>/dev/null && wait $child; rm -f $pid /tmp/q2-airplay.bin /tmp/q2-airplay.cliairplay /tmp/q2-airplay.run; exit 0' TERM INT
    delay=5
    while :; do
        /tmp/q2-airplay.bin -dir "$dir" -sender /tmp/q2-airplay.cliairplay &
        child=$!
        wait $child
        code=$?
        [ $code = 0 ] && break
        sleep $delay
        [ $delay -lt 60 ] && delay=$((delay * 2))
    done
    rm -f $pid /tmp/q2-airplay.bin /tmp/q2-airplay.cliairplay /tmp/q2-airplay.run
) </dev/null >/dev/null 2>&1 &
echo $! > $pid
