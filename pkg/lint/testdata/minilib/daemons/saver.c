inherit "/std/thing";

private int save_me;
static int lost_counter;    /* BUG: static var in auto-saving object */

static void
create()
{
    ::create();
    set_auto_save(1);
}
