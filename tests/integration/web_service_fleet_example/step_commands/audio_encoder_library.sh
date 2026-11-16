#!/bin/sh
# Moves the MP3 library that the audio encoder lame needs out of the way, so
# that lame cannot start, or puts it back.
#   sh step_commands/audio_encoder_library.sh remove|restore
set -eu
library=$(dpkg -L libmp3lame0 | grep '/libmp3lame\.so\.0$')
case $1 in
  remove) mv "$library" "$library.moved-away-by-the-test" ;;
  restore) mv "$library.moved-away-by-the-test" "$library" ;;
  *) echo "unknown action $1: remove or restore" >&2; exit 2 ;;
esac
