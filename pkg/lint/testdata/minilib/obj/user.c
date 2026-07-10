#include <paths.h>

inherit STD_THING;

static void
create()
{
    ::create();
    call_out("tick", 10);
    call_out("gone_callback", 10);          /* BUG: no such function */
    register_handler("no_handler");         /* BUG via config registrar */
    register_handler("on_event");           /* ok */
}

static void
tick()
{
    this_object()->visible_fn();
    this_object()->missing_fn();            /* BUG: callable-not-found */
    this_object()->hidden_fn();             /* ok: static + same object is legal */
    this_object()->secret_fn();             /* BUG: private is never reachable */
    this_object()->object_name();           /* ok: auto object provides it */
    call_other(this_object(), "visible_fn");
    call_other(this_object(), "also_gone"); /* BUG: callable-not-found */
    "/std/thing"->visible_fn();
    "/std/thing"->nothere_fn();             /* BUG: callable-not-found */
    "/std/thing"->hidden_fn();              /* BUG: static cross-object */
    never_defined();                        /* BUG: undefined-prototype */
    clone_object("/obj/nothing");           /* BUG: target-object-missing */
    "/std/gone"->poke();                    /* BUG: target-object-missing */
    "/virtual/room1"->probe();              /* ok: virtual path */
}

public void
on_event()
{
}
