#define _GNU_SOURCE
#include <dirent.h>
#include <errno.h>
#include <fcntl.h>
#include <signal.h>
#include <stdio.h>
#include <string.h>
#include <sys/types.h>
#include <unistd.h>

#include <lxc/lxccontainer.h>

/*
/ Set to 0 to disable use of lxc-init.
/ The container process should have PID 1.
*/
#define ENABLE_LXCINIT 0

#define ERROR(format, ...)                                                     \
	{                                                                      \
		fprintf(stderr, "[lxcri-start] " format, ##__VA_ARGS__);       \
		ret = EXIT_FAILURE;                                            \
		goto out;                                                      \
	}

/* NOTE lxc_execute.c was taken as guidline and some lines where copied. */
int main(int argc, char **argv)
{
	struct lxc_container *c = NULL;
	int ret = EXIT_SUCCESS;
	const char *name;
	const char *lxcpath;
	const char *rcfile;

	/* Ensure stdout and stderr are line bufferd. */
	setvbuf(stdout, NULL, _IOLBF, -1);
	setvbuf(stderr, NULL, _IOLBF, -1);
	errno = 0;

	if (argc != 4)
		ERROR("invalid argument count, usage: "
		      "$0 <container_name> <lxcpath> <config_path>\n");

	/*
	/ If this is non interactive, get rid of our controlling terminal,
	/ since we don't want lxc's setting of ISIG to ignore user's ^Cs.
	/ Ignore any error - because controlling terminal could be a PTY.
	*/
	setsid();
	errno = 0;

	name = argv[1];
	lxcpath = argv[2];
	rcfile = argv[3];

	/* Close all file descriptors but stdio (0,1,2).
	 * To pass additional file descriptors to the runtime
	 * in order to enable [socket activation][systemd-listen-fds]
	 * the environment variable LISTEN_FDS can be used.
	 * If LISTEN_FDS is set to a value n > 0, than all
	 * filedescriptors > 2+n are kept open.
	 */

	int procfd;
	DIR *dirp = NULL;
	struct dirent *entry = NULL;
	int keepfds = 0;
	char *env_listen = getenv("LISTEN_FDS");

	if (env_listen != NULL)
		keepfds = atoi(env_listen);

	procfd = open("/proc/self/fd", O_RDONLY | O_CLOEXEC);
	if (procfd == -1)
		ERROR("open /proc/self/fd failed");

	dirp = fdopendir(procfd);
	if (dirp == NULL)
		ERROR("fdopendir for /proc/self/fd failed");

	while ((entry = readdir(dirp)) != NULL) {
		errno = 0; // reset errno from previous strtol or close
		int xfd = strtol(entry->d_name, NULL, 10);
		if (errno)
			continue;

		if ((xfd > 2 + keepfds) && (xfd != procfd))
			close(xfd);
	}

	closedir(dirp);

	c = lxc_container_new(name, lxcpath);
	if (c == NULL)
		ERROR("failed to create new container");

	c->clear_config(c);

	if (!c->load_config(c, rcfile))
		ERROR("failed to load container config %s\n", rcfile);

	/* Do not daemonize - this would null the inherited stdio. */
	c->daemonize = false;
	c->start(c, ENABLE_LXCINIT, NULL);

	/* Try to die with the same signal the task did. */
	/* FIXME error_num is zero if init was killed with SIGHUP */
	if (WIFSIGNALED(c->error_num))
		kill(0, WTERMSIG(c->error_num));

	if (WIFEXITED(c->error_num))
		ret = WEXITSTATUS(c->error_num);
out:
	if (c != NULL)
		lxc_container_put(c);
	exit(ret);
}
