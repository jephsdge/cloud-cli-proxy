#!/bin/sh
# v3.0 baseline — Claude Code truecolor hint (PITFALLS M8)
export CLAUDE_CODE_TMUX_TRUECOLOR=1

case "$-" in
  *i*) ;;
  *) return 0 2>/dev/null || exit 0 ;;
esac

alias ll='ls -alF --color=auto'
alias la='ls -A --color=auto'
alias l='ls -CF --color=auto'
