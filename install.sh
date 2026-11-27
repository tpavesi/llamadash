#!/bin/sh
# llamadash installer: asks for every setting, then installs the binary, writes
# the settings file and, if wanted, sets up the boot service on the console.
#
# Run it as root on the machine that runs llama-server, from the directory that
# holds the built files (dist/ after "python build.py"):
#
#     sudo sh install.sh
#
# Answers already in the settings file are offered as the defaults, so running it
# again changes one thing without retyping the rest. Nothing is written until the
# summary at the end is confirmed. The one thing done before that, and only if
# agreed to, is the fan sensor check: it loads kernel sensor drivers to see
# which one reports fan speeds, and unloads them again if the answer is no.
#
# Environment overrides (for packaging or tests): PREFIX (default /usr/local),
# CONF (default /etc/llamadash.conf), UNIT_DIR (default /etc/systemd/system),
# MODLOAD_DIR (/etc/modules-load.d), MODPROBE_DIR (/etc/modprobe.d),
# HWMON (/sys/class/hwmon).

set -eu

PREFIX=${PREFIX:-/usr/local}
CONF=${CONF:-/etc/llamadash.conf}
UNIT_DIR=${UNIT_DIR:-/etc/systemd/system}
MODLOAD_DIR=${MODLOAD_DIR:-/etc/modules-load.d}
MODPROBE_DIR=${MODPROBE_DIR:-/etc/modprobe.d}
HWMON=${HWMON:-/sys/class/hwmon}
BIN=$PREFIX/bin/llamadash
HERE=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)

say() { printf '%s\n' "$*"; }
die() { printf 'install.sh: %s\n' "$*" >&2; exit 1; }

# ask VAR "question" "default": reads one line; an empty answer keeps the default.
ask() {
	_def=$3
	if [ -n "$_def" ]; then
		printf '%s [%s]: ' "$2" "$_def"
	else
		printf '%s: ' "$2"
	fi
	IFS= read -r _ans || die "no more input"
	[ -n "$_ans" ] || _ans=$_def
	eval "$1=\$_ans"
}

# ask_yn VAR "question" "y|n"
ask_yn() {
	while :; do
		ask _yn "$2 (y/n)" "$3"
		case $_yn in
		y | Y | yes | YES) eval "$1=y"; return ;;
		n | N | no | NO) eval "$1=n"; return ;;
		esac
		say "  Please answer y or n."
	done
}

# conf_get NAME: the value of NAME in the existing settings file, if any.
conf_get() {
	[ -r "$CONF" ] || return 0
	sed -n "s/^[[:space:]]*$1[[:space:]]*=[[:space:]]*//p" "$CONF" | tail -n 1 | sed "s/^[\"']//; s/[\"']\$//"
}

# ---- what is being installed -------------------------------------------------

[ "$(id -u)" = 0 ] || die "run this as root (sudo sh install.sh)"

case $(uname -m) in
x86_64 | amd64) ARCH=amd64 ;;
aarch64 | arm64) ARCH=arm64 ;;
*) die "unsupported machine type $(uname -m): llamadash is built for amd64 and arm64" ;;
esac

SRC=
for f in "$HERE/llamadash-linux-$ARCH" "$HERE/dist/llamadash-linux-$ARCH"; do
	if [ -f "$f" ]; then
		SRC=$f
		break
	fi
done
[ -n "$SRC" ] || die "llamadash-linux-$ARCH not found next to this script; build it first with: python build.py"

UNIT_SRC=
for f in "$HERE/contrib/llamadash.service" "$HERE/dist/contrib/llamadash.service"; do
	if [ -f "$f" ]; then
		UNIT_SRC=$f
		break
	fi
done

TMP=$(mktemp -d)
trap 'rm -rf "$TMP"' EXIT INT TERM
cp "$SRC" "$TMP/llamadash"
chmod 755 "$TMP/llamadash"
# --config /dev/null: a bad line in the existing settings file must not stop the installer
VERSION=$("$TMP/llamadash" --config /dev/null --version 2>/dev/null) || die "$SRC does not run on this machine"

say "llamadash installer ($VERSION, linux/$ARCH)"
say "Press Enter to keep the value in [brackets]."
say

# ---- questions ---------------------------------------------------------------

d_url=$(conf_get LLAMADASH_URL)
d_key=$(conf_get LLAMA_API_KEY)
d_price=$(conf_get LLAMADASH_KWH_PRICE)
d_cur=$(conf_get LLAMADASH_CURRENCY)
d_db=$(conf_get LLAMADASH_DB)
d_psu=$(conf_get LLAMADASH_PSU_WATTS)

while :; do
	say "1. llama-server address"
	say "   The base URL the server listens on, as seen from this machine."
	ask URL "   URL" "${d_url:-http://localhost:8080}"

	say
	say "2. API key"
	say "   Only if the server was started with one. Type - for none."
	if [ -n "$d_key" ]; then
		ask KEY "   API key (Enter keeps the stored key)" ""
		[ -n "$KEY" ] || KEY=$d_key
	else
		ask KEY "   API key" "-"
	fi
	[ "$KEY" != "-" ] || KEY=

	say
	say "3. Electricity price"
	say "   Price of one kWh as a plain number, for example 0.30. It turns on the"
	say "   cost figures. Type - for no cost figures."
	ask PRICE "   Price per kWh" "${d_price:--}"
	[ "$PRICE" != "-" ] || PRICE=
	CUR=
	if [ -n "$PRICE" ]; then
		say "   Three-letter code of the currency that price is in, for example GBP,"
		say "   EUR or USD."
		ask CUR "   Currency" "${d_cur:-GBP}"
		[ "$CUR" != "-" ] || CUR=
	fi

	say
	say "4. Power supply"
	say "   Maximum wattage of this machine's power supply, for example 1000. It"
	say "   fixes the top of the power chart. Type - to skip."
	ask PSU "   Watts" "${d_psu:--}"
	[ "$PSU" != "-" ] || PSU=

	say
	say "5. History database"
	say "   Where energy, cost and request history is kept. Type - for the default"
	say "   place (/var/lib/llamadash/llamadash.db when run as root)."
	ask DB "   Database file" "${d_db:--}"
	[ "$DB" != "-" ] || DB=

	# let the program itself judge the answers
	{
		say "LLAMADASH_URL=$URL"
		[ -z "$KEY" ] || say "LLAMA_API_KEY=$KEY"
		[ -z "$PRICE" ] || say "LLAMADASH_KWH_PRICE=$PRICE"
		[ -z "$CUR" ] || say "LLAMADASH_CURRENCY=$CUR"
		[ -z "$PSU" ] || say "LLAMADASH_PSU_WATTS=$PSU"
		[ -z "$DB" ] || say "LLAMADASH_DB=$DB"
	} >"$TMP/check.conf"
	say
	say "Checking the settings against the server ..."
	rc=0
	"$TMP/llamadash" --config "$TMP/check.conf" --once --no-db --no-gpu >"$TMP/once.out" 2>"$TMP/once.err" || rc=$?
	case $rc in
	0)
		say "  ok: $(head -n 1 "$TMP/once.out")"
		break
		;;
	2)
		say "  A setting was not accepted:"
		sed 's/^/    /' "$TMP/once.err"
		say "  Please answer the questions again."
		say
		d_url=$URL d_key=$KEY d_price=$PRICE d_cur=$CUR d_db=$DB d_psu=$PSU
		;;
	*)
		say "  The settings are valid, but the server did not answer at $URL."
		say "  (That is fine if it is simply not running at the moment.)"
		ask_yn KEEP "  Keep this address" y
		[ "$KEEP" = n ] || break
		say
		d_url=$URL d_key=$KEY d_price=$PRICE d_cur=$CUR d_db=$DB d_psu=$PSU
		;;
	esac
done

# The chart scales the dashboard learned (LLAMADASH_AXIS_* lines, which it writes
# itself) are carried over, each only if the program accepts it.
SCALES_NOTE="# Chart scales learned by llamadash (it raises these itself)."
AXIS=
for _k in LLAMADASH_AXIS_POWER_W LLAMADASH_AXIS_GEN_TPS LLAMADASH_AXIS_PROMPT_TPS; do
	_v=$(conf_get "$_k")
	[ -n "$_v" ] || continue
	say "$_k=$_v" >"$TMP/axis.conf"
	# the program ignores a bad one with a warning: any warning means drop it
	if "$TMP/llamadash" --config "$TMP/axis.conf" --version >/dev/null 2>"$TMP/axis.err" && [ ! -s "$TMP/axis.err" ]; then
		AXIS="$AXIS$_k=$_v
"
	else
		say "  (the learned chart scale $_k=$_v is not valid and is dropped)"
	fi
done

# The fans the dashboard has seen spinning (also written by itself), kept the
# same way: dropped when the program warns about the line.
FANS_NOTE="# Fans llamadash has seen spinning (it adds to this itself; delete an entry to forget a fan)."
FANS=$(conf_get LLAMADASH_KNOWN_FANS)
if [ -n "$FANS" ]; then
	say "LLAMADASH_KNOWN_FANS=$FANS" >"$TMP/fans.conf"
	if ! "$TMP/llamadash" --config "$TMP/fans.conf" --version >/dev/null 2>"$TMP/fans.err" || [ -s "$TMP/fans.err" ]; then
		say "  (the known fans line LLAMADASH_KNOWN_FANS=$FANS is not valid and is dropped)"
		FANS=
	fi
fi

# The status lighting settings (README, "Status lighting") are not asked about;
# lines already in the settings file are kept, each only if the program accepts it.
LED_NOTE="# Status lighting through OpenRGB (see the README, \"Status lighting\")."
LED=
for _k in LLAMADASH_LED LLAMADASH_LED_CMD LLAMADASH_LED_DEVICE; do
	_v=$(conf_get "$_k")
	[ -n "$_v" ] || continue
	say "$_k=$_v" >"$TMP/led.conf"
	if "$TMP/llamadash" --config "$TMP/led.conf" --version >/dev/null 2>&1; then
		LED="$LED$_k=$_v
"
	else
		say "  (the lighting setting $_k=$_v is not valid and is dropped)"
	fi
done

SERVICE=n
START=n
if [ -n "$UNIT_SRC" ] && command -v systemctl >/dev/null 2>&1; then
	say
	say "6. Start at boot"
	say "   Show the dashboard on this machine's own monitor (the first console,"
	say "   tty1) from boot, in place of the login prompt there. Other consoles"
	say "   (Alt+F2) keep their login prompt."
	ask_yn SERVICE "   Install the boot service" n
	if [ "$SERVICE" = y ]; then
		ask_yn START "   Start it on the monitor now" y
	fi
else
	say
	say "6. Start at boot: skipped (no systemd here, or llamadash.service not found)."
fi

# ---- fan sensors -------------------------------------------------------------

# fan_count: how many motherboard fan speed readings the kernel offers now.
fan_count() {
	_n=0
	for _f in "$HWMON"/hwmon*/fan*_input; do
		[ -r "$_f" ] || continue
		case $(cat "${_f%/*}/name" 2>/dev/null) in
		nvidia | amdgpu | nouveau) continue ;;
		esac
		_n=$((_n + 1))
	done
	echo "$_n"
}

# Drivers for the sensor chips of common boards, tried in this order. The last
# one tells the nct6683 driver to read a chip on a board maker it does not know
# (several ASRock boards on older kernels); it reads sensors only.
FAN_DRIVERS='nct6775
it87
nct6683
f71882fg
w83627ehf
nct6683 force=1'

FAN_MOD=
FAN_OPT=
FAN_LOADED=n
say
say "7. Fan sensors"
fans=$(fan_count)
if [ "$fans" -gt 0 ]; then
	say "   $fans motherboard fan readings are available. Nothing to do."
elif ! command -v modprobe >/dev/null 2>&1 || [ ! -d "$HWMON" ]; then
	say "   Skipped (no modprobe or no $HWMON here)."
else
	say "   The kernel offers no motherboard fan speeds, so the dashboard would show"
	say "   no fans row. The installer can try the usual sensor drivers one by one"
	say "   (it loads each, looks for fan speeds, and unloads it again if there are"
	say "   none). They only read sensors."
	ask_yn PROBE "   Look for a fan sensor driver" y
	if [ "$PROBE" = y ]; then
		OLDIFS=$IFS
		IFS='
'
		for _line in $FAN_DRIVERS; do
			IFS=$OLDIFS
			_mod=${_line%% *}
			_opt=
			[ "$_mod" = "$_line" ] || _opt=${_line#* }
			modprobe -n "$_mod" >/dev/null 2>&1 || continue
			_was=n
			[ ! -d "/sys/module/$_mod" ] || _was=y
			# an option only takes effect on a fresh load
			if [ "$_was" = y ] && [ -n "$_opt" ]; then
				modprobe -r "$_mod" >/dev/null 2>&1 || continue
				_was=n
			fi
			# shellcheck disable=SC2086
			if modprobe "$_mod" $_opt >/dev/null 2>&1; then
				sleep 1
				fans=$(fan_count)
				if [ "$fans" -gt 0 ]; then
					FAN_MOD=$_mod FAN_OPT=$_opt
					[ "$_was" = y ] || FAN_LOADED=y
					break
				fi
				[ "$_was" = y ] || modprobe -r "$_mod" >/dev/null 2>&1 || true
			fi
		done
		IFS=$OLDIFS
		if [ -n "$FAN_MOD" ]; then
			say "   Found $fans fan readings with the driver: $FAN_MOD${FAN_OPT:+ $FAN_OPT}"
			ask_yn FAN_KEEP "   Load this driver at every boot" y
			if [ "$FAN_KEEP" = n ]; then
				[ "$FAN_LOADED" = n ] || modprobe -r "$FAN_MOD" >/dev/null 2>&1 || true
				FAN_MOD=
			fi
		else
			say "   None of the usual drivers found fan speeds on this board. The"
			say "   dashboard works without them; \"sensors-detect\" (package lm-sensors)"
			say "   can look further."
		fi
	fi
fi

# ---- summary -----------------------------------------------------------------

say
say "About to do this:"
say "  install   $BIN  ($VERSION)"
say "  write     $CONF"
say "              LLAMADASH_URL=$URL"
[ -z "$KEY" ] || say "              LLAMA_API_KEY=(set; the file will be readable by root only)"
[ -z "$PRICE" ] || say "              LLAMADASH_KWH_PRICE=$PRICE"
[ -z "$CUR" ] || say "              LLAMADASH_CURRENCY=$CUR"
[ -z "$PSU" ] || say "              LLAMADASH_PSU_WATTS=$PSU"
[ -z "$DB" ] || say "              LLAMADASH_DB=$DB"
[ -z "$AXIS" ] || printf '%s' "$AXIS" | sed 's/^/              /; s/$/  (learned chart scale, kept)/'
[ -z "$FANS" ] || say "              LLAMADASH_KNOWN_FANS=$FANS  (fans seen spinning, kept)"
[ -z "$LED" ] || printf '%s' "$LED" | sed 's/^/              /; s/$/  (status lighting, kept)/'
if [ "$SERVICE" = y ]; then
	say "  install   $UNIT_DIR/llamadash.service, enabled at boot"
	[ "$START" = n ] || say "  start     the dashboard on tty1 now (the login prompt there stops)"
fi
if [ -n "$FAN_MOD" ]; then
	say "  write     $MODLOAD_DIR/llamadash.conf  (loads the fan sensor driver $FAN_MOD at boot)"
	[ -z "$FAN_OPT" ] || say "  write     $MODPROBE_DIR/llamadash.conf  (options $FAN_MOD $FAN_OPT)"
fi
[ ! -f "$CONF" ] || say "  (the existing $CONF is kept as $CONF.bak)"
ask_yn GO "Proceed" y
if [ "$GO" != y ]; then
	[ -z "$FAN_MOD" ] || [ "$FAN_LOADED" = n ] || modprobe -r "$FAN_MOD" >/dev/null 2>&1 || true
	die "nothing was changed"
fi

# ---- do it -------------------------------------------------------------------

install -d "$PREFIX/bin"
install -m 755 "$SRC" "$BIN"

[ ! -f "$CONF" ] || cp -p "$CONF" "$CONF.bak"
mode=644
[ -z "$KEY" ] || mode=600
{
	say "# Settings for llamadash, written by install.sh. Lines are NAME=value."
	say "# A flag on the command line or a variable in the environment overrides"
	say "# the line here. After a change: systemctl restart llamadash"
	say
	say "# Address of the llama-server."
	say "LLAMADASH_URL=$URL"
	if [ -n "$KEY" ]; then
		say
		say "# API key of the llama-server. Keep this file readable by root only."
		say "LLAMA_API_KEY=$KEY"
	fi
	if [ -n "$PRICE" ]; then
		say
		say "# Price of one kWh, and the three-letter code of its currency."
		say "LLAMADASH_KWH_PRICE=$PRICE"
		[ -z "$CUR" ] || say "LLAMADASH_CURRENCY=$CUR"
	fi
	if [ -n "$PSU" ]; then
		say
		say "# Maximum wattage of the power supply: the top of the power chart."
		say "LLAMADASH_PSU_WATTS=$PSU"
	fi
	if [ -n "$DB" ]; then
		say
		say "# History database file."
		say "LLAMADASH_DB=$DB"
	fi
	if [ -n "$AXIS" ]; then
		say
		say "$SCALES_NOTE"
		printf '%s' "$AXIS"
	fi
	if [ -n "$FANS" ]; then
		say
		say "$FANS_NOTE"
		say "LLAMADASH_KNOWN_FANS=$FANS"
	fi
	if [ -n "$LED" ]; then
		say
		say "$LED_NOTE"
		printf '%s' "$LED"
	fi
} >"$TMP/llamadash.conf"
install -m "$mode" "$TMP/llamadash.conf" "$CONF"

if [ -n "$FAN_MOD" ]; then
	install -d "$MODLOAD_DIR"
	{
		say "# Fan sensor driver for llamadash, written by install.sh."
		say "$FAN_MOD"
	} >"$TMP/modload.conf"
	install -m 644 "$TMP/modload.conf" "$MODLOAD_DIR/llamadash.conf"
	if [ -n "$FAN_OPT" ]; then
		install -d "$MODPROBE_DIR"
		{
			say "# Written by install.sh: lets the driver read this board's sensor chip."
			say "options $FAN_MOD $FAN_OPT"
		} >"$TMP/modprobe.conf"
		install -m 644 "$TMP/modprobe.conf" "$MODPROBE_DIR/llamadash.conf"
	fi
fi

if [ "$SERVICE" = y ]; then
	install -d "$UNIT_DIR"
	sed "s|^ExecStart=.*|ExecStart=$BIN|" "$UNIT_SRC" >"$TMP/llamadash.service"
	install -m 644 "$TMP/llamadash.service" "$UNIT_DIR/llamadash.service"
	systemctl daemon-reload
	if [ "$START" = y ]; then
		systemctl enable llamadash
		systemctl restart llamadash
	else
		systemctl enable llamadash
	fi
fi

say
say "Done."
say "  Run it by hand:      llamadash        (as root for CPU power and the container line)"
say "  Change a setting:    edit $CONF, or run this installer again"
if [ "$SERVICE" = y ]; then
	say "  Boot service:        systemctl status llamadash"
	say "  Login prompt back:   Alt+F2, or: systemctl stop llamadash && systemctl start getty@tty1"
fi
[ -z "$FAN_MOD" ] || say "  Fan sensor driver:   $MODLOAD_DIR/llamadash.conf (delete it to stop loading $FAN_MOD)"
say "  Remove everything:  see \"Uninstall\" in the README"
